/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package validation

import (
	"reflect"

	"k8s.io/apiserver/pkg/cel/common"
	celopenapi "k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
)

// schemaArgs are the arguments to constructor for OpenAPI schema validator,
// NewSchemaValidator
type schemaArgs struct {
	schema       *spec.Schema
	root         interface{}
	path         string
	knownFormats strfmt.Registry
	options      []validate.Option
}

// RatchetingSchemaValidator wraps kube-openapis SchemaValidator to provide a
// ValidateUpdate function which allows ratcheting
type RatchetingSchemaValidator struct {
	schemaArgs
}

func NewRatchetingSchemaValidator(schema *spec.Schema, rootSchema interface{}, root string, formats strfmt.Registry, options ...validate.Option) *RatchetingSchemaValidator {
	return &RatchetingSchemaValidator{
		schemaArgs: schemaArgs{
			schema:       schema,
			root:         rootSchema,
			path:         root,
			knownFormats: formats,
			options:      options,
		},
	}
}

func (r *RatchetingSchemaValidator) Validate(new interface{}) *validate.Result {
	sv := validate.NewSchemaValidator(r.schema, r.root, r.path, r.knownFormats, r.options...)
	return sv.Validate(new)
}

func (r *RatchetingSchemaValidator) ValidateUpdate(new, old interface{}) *validate.Result {
	return newRatchetingValueValidator(NewCorrelatedObject(new, old, r.schema), r.schemaArgs).Validate(new)
}

// ratchetingValueValidator represents an invocation of SchemaValidator.ValidateUpdate
// for specific arguments for `old` and `new`
//
// It follows the openapi SchemaValidator down its traversal of the new value
// by injecting validate.Option into each recursive invocation.
//
// A ratchetingValueValidator will be constructed and added to the tree for
// each explored sub-index and sub-property during validation.
//
// It's main job is to keep the old/new values correlated as the traversal
// continues, and postprocess errors according to our ratcheting policy.
//
// ratchetingValueValidator is not thread safe.
type ratchetingValueValidator struct {
	// schemaArgs provides the arguments to use in the temporary SchemaValidator
	// that is created during a call to Validate.
	schemaArgs
	correlation *CorrelatedObject
}

type CorrelatedObject struct {
	// Currently correlated old value during traversal of the schema/object
	OldValue interface{}

	// Value being validated
	Value interface{}

	Schema *spec.Schema

	// Scratch space below, may change during validation

	// Cached comparison result of DeepEqual of `value` and `thunk.oldValue`
	comparisonResult *bool

	// Cached map representation of a map-type list, or nil if not map-type list
	mapList common.MapList

	// Children spawned by a call to `Validate` on this object
	// key is either a string or an index, depending upon whether `value` is
	// a map or a list, respectively.
	//
	// The list of children may be incomplete depending upon if the internal
	// logic of kube-openapi's SchemaValidator short-circuited before
	// reaching all of the children.
	//
	// It should be expected to have an entry for either all of the children, or
	// none of them.
	children map[interface{}]*CorrelatedObject
}

func NewCorrelatedObject(new, old interface{}, schema *spec.Schema) *CorrelatedObject {
	return &CorrelatedObject{
		OldValue: old,
		Value:    new,
		Schema:   schema,
	}
}

func newRatchetingValueValidator(correlation *CorrelatedObject, args schemaArgs) *ratchetingValueValidator {
	return &ratchetingValueValidator{
		schemaArgs:  args,
		correlation: correlation,
	}
}

// getValidateOption provides a kube-openapi validate.Option for SchemaValidator
// that injects a ratchetingValueValidator to be used for all subkeys and subindices
func (r *ratchetingValueValidator) getValidateOption() validate.Option {
	return func(svo *validate.SchemaValidatorOptions) {
		svo.NewValidatorForField = r.SubPropertyValidator
		svo.NewValidatorForIndex = r.SubIndexValidator
	}
}

// Validate validates the update from r.oldValue to r.value
//
// During evaluation, a temporary tree of ratchetingValueValidator is built for all
// traversed field paths. It is necessary to build the tree to take advantage of
// DeepEqual checks performed by lower levels of the object during validation without
// greatly modifying `kube-openapi`'s implementation.
//
// The tree, and all cache storage/scratch space for the validation of a single
// call to `Validate` is thrown away at the end of the top-level call
// to `Validate`.
//
// `Validate` will create a node in the tree to for each of the explored children.
// The node's main purpose is to store a lazily computed DeepEqual check between
// the oldValue and the currently passed value. If the check is performed, it
// will be stored in the node to be re-used by a parent node during a DeepEqual
// comparison, if necessary.
//
// This call has a side-effect of populating it's `children` variable with
// the explored nodes of the object tree.
func (r *ratchetingValueValidator) Validate(new interface{}) *validate.Result {
	opts := append([]validate.Option{
		r.getValidateOption(),
	}, r.options...)

	s := validate.NewSchemaValidator(r.schema, r.root, r.path, r.knownFormats, opts...)

	res := s.Validate(r.correlation.Value)

	if res.IsValid() {
		return res
	}

	// Current ratcheting rule is to ratchet errors if DeepEqual(old, new) is true.
	if r.correlation.CachedDeepEqual() {
		newRes := &validate.Result{}
		newRes.MergeAsWarnings(res)
		return newRes
	}

	return res
}

// SubPropertyValidator overrides the standard validator constructor for sub-properties by
// returning our special ratcheting variant.
//
// If we can correlate an old value, we return a ratcheting validator to
// use for the child.
//
// If the old value cannot be correlated, then default validation is used.
func (r *ratchetingValueValidator) SubPropertyValidator(field string, schema *spec.Schema, rootSchema interface{}, root string, formats strfmt.Registry, options ...validate.Option) validate.ValueValidator {
	childNode := r.correlation.Key(field)
	if childNode == nil {
		return validate.NewSchemaValidator(schema, rootSchema, root, formats, options...)
	}

	return newRatchetingValueValidator(childNode, schemaArgs{
		schema:       schema,
		root:         rootSchema,
		path:         root,
		knownFormats: formats,
		options:      options,
	})
}

// SubIndexValidator overrides the standard validator constructor for sub-indicies by
// returning our special ratcheting variant.
//
// If we can correlate an old value, we return a ratcheting validator to
// use for the child.
//
// If the old value cannot be correlated, then default validation is used.
func (r *ratchetingValueValidator) SubIndexValidator(index int, schema *spec.Schema, rootSchema interface{}, root string, formats strfmt.Registry, options ...validate.Option) validate.ValueValidator {
	childNode := r.correlation.Index(index)
	if childNode == nil {
		return validate.NewSchemaValidator(schema, rootSchema, root, formats, options...)
	}

	return newRatchetingValueValidator(childNode, schemaArgs{
		schema:       schema,
		root:         rootSchema,
		path:         root,
		knownFormats: formats,
		options:      options,
	})
}

// If oldValue is not a list, returns nil
// If oldValue is a list takes mapType into account and attempts to find the
// old value with the same index or key, depending upon the mapType.
//
// If listType is map, creates a map representation of the list using the designated
// map-keys and caches it for future calls.
func (r *CorrelatedObject) correlateOldValueForChildAtNewIndex(index int) any {
	oldAsList, ok := r.OldValue.([]interface{})
	if !ok {
		return nil
	}

	asList, ok := r.Value.([]interface{})
	if !ok {
		return nil
	} else if len(asList) <= index {
		// Cannot correlate out of bounds index
		return nil
	}

	listType, _ := r.Schema.Extensions.GetString("x-kubernetes-list-type")
	switch listType {
	case "map":
		// Look up keys for this index in current object
		currentElement := asList[index]

		oldList := r.mapList
		if oldList == nil {
			oldList = celopenapi.MakeMapList(r.Schema, oldAsList)
			r.mapList = oldList
		}
		return oldList.Get(currentElement)

	case "set":
		// Are sets correlatable? Only if the old value equals the current value.
		// We might be able to support this, but do not currently see a lot
		// of value
		// (would allow you to add/remove items from sets with ratcheting but not change them)
		return nil
	case "atomic":
		// Atomic lists are not correlatable by item
		// Ratcheting is not available on a per-index basis
		return nil
	default:
		// Correlate by-index by default.
		//
		// Cannot correlate an out-of-bounds index
		if len(oldAsList) <= index {
			return nil
		}

		return oldAsList[index]
	}
}

// CachedDeepEqual is equivalent to reflect.DeepEqual, but caches the
// results in the tree of ratchetInvocationScratch objects on the way:
//
// For objects and arrays, this function will make a best effort to make
// use of past DeepEqual checks performed by this Node's children, if available.
//
// If a lazy computation could not be found for all children possibly due
// to validation logic short circuiting and skipping the children, then
// this function simply defers to reflect.DeepEqual.
func (r *CorrelatedObject) CachedDeepEqual() (res bool) {
	if r.comparisonResult != nil {
		return *r.comparisonResult
	}

	defer func() {
		r.comparisonResult = &res
	}()

	if r.Value == nil && r.OldValue == nil {
		return true
	} else if r.Value == nil || r.OldValue == nil {
		return false
	}

	oldAsArray, oldIsArray := r.OldValue.([]interface{})
	newAsArray, newIsArray := r.Value.([]interface{})

	if oldIsArray != newIsArray {
		return false
	} else if oldIsArray {
		if len(oldAsArray) != len(newAsArray) {
			return false
		} else if len(r.children) != len(oldAsArray) {
			// kube-openapi validator is written to always visit all
			// children of a slice, so this case is only possible if
			// one of the children could not be correlated. In that case,
			// we know the objects are not equal.
			//
			return false
		}

		// Correctly considers map-type lists due to fact that index here
		// is only used for numbering. The correlation is stored in the
		// childInvocation itself
		//
		// NOTE: This does not consider sets, since we don't correlate them.
		for i := range newAsArray {
			// Query for child
			child, ok := r.children[i]
			if !ok {
				// This should not happen
				return false
			} else if !child.CachedDeepEqual() {
				// If one child is not equal the entire object is not equal
				return false
			}
		}

		return true
	}

	oldAsMap, oldIsMap := r.OldValue.(map[string]interface{})
	newAsMap, newIsMap := r.Value.(map[string]interface{})

	if oldIsMap != newIsMap {
		return false
	} else if oldIsMap {
		if len(oldAsMap) != len(newAsMap) {
			return false
		} else if len(oldAsMap) == 0 && len(newAsMap) == 0 {
			// Both empty
			return true
		} else if len(r.children) != len(oldAsMap) {
			// If we are missing a key it is because the old value could not
			// be correlated to the new, so the objects are not equal.
			//
			return false
		}

		for k := range oldAsMap {
			// Check to see if this child was explored during validation
			child, ok := r.children[k]
			if !ok {
				// Child from old missing in new due to key change
				// Objects are not equal.
				return false
			} else if !child.CachedDeepEqual() {
				// If one child is not equal the entire object is not equal
				return false
			}
		}

		return true
	}

	return reflect.DeepEqual(r.OldValue, r.Value)
}

var _ validate.ValueValidator = (&ratchetingValueValidator{})

func (f ratchetingValueValidator) SetPath(path string) {
	// Do nothing
	// Unused by kube-openapi
}

func (f ratchetingValueValidator) Applies(source interface{}, valueKind reflect.Kind) bool {
	return true
}

// Key returns the child of the reciever with the given name.
// Returns nil if the given name is does not exist in the new object, or its
// value is not correlatable to an old value.
// If receiver is nil or if the new value is not an object/map, returns nil.
func (l *CorrelatedObject) Key(field string) *CorrelatedObject {
	if l == nil || l.Schema == nil {
		return nil
	} else if existing, exists := l.children[field]; exists {
		return existing
	}

	// Find correlated old value
	oldAsMap, okOld := l.OldValue.(map[string]interface{})
	newAsMap, okNew := l.Value.(map[string]interface{})
	if !okOld || !okNew {
		return nil
	}

	oldValueForField, okOld := oldAsMap[field]
	newValueForField, okNew := newAsMap[field]
	if !okOld || !okNew {
		return nil
	}

	var propertySchema *spec.Schema
	if prop, exists := l.Schema.Properties[field]; exists {
		propertySchema = &prop
	} else if addP := l.Schema.AdditionalProperties; addP != nil && addP.Schema != nil {
		propertySchema = addP.Schema
	} else {
		return nil
	}

	if l.children == nil {
		l.children = make(map[interface{}]*CorrelatedObject, len(newAsMap))
	}

	res := NewCorrelatedObject(newValueForField, oldValueForField, propertySchema)
	l.children[field] = res
	return res
}

// Index returns the child of the reciever at the given index.
// Returns nil if the given index is out of bounds, or its value is not
// correlatable to an old value.
// If receiver is nil or if the new value is not an array, returns nil.
func (l *CorrelatedObject) Index(i int) *CorrelatedObject {
	if l == nil || l.Schema == nil {
		return nil
	} else if existing, exists := l.children[i]; exists {
		return existing
	}

	asList, ok := l.Value.([]interface{})
	if !ok || len(asList) <= i {
		return nil
	}

	oldValueForIndex := l.correlateOldValueForChildAtNewIndex(i)
	if oldValueForIndex == nil {
		return nil
	}
	var itemSchema *spec.Schema
	if i := l.Schema.Items; i != nil && i.Schema != nil {
		itemSchema = i.Schema
	} else {
		return nil
	}

	if l.children == nil {
		l.children = make(map[interface{}]*CorrelatedObject, len(asList))
	}

	res := NewCorrelatedObject(asList[i], oldValueForIndex, itemSchema)
	l.children[i] = res
	return res
}
