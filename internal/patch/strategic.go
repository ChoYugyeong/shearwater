/*
Copyright 2014 The Kubernetes Authors.
Copyright 2026 The Shearwater Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Strategic merge behavior is adapted from k8s.io/apimachinery v0.36.6:
https://github.com/kubernetes/apimachinery/blob/v0.36.6/pkg/util/strategicpatch/patch.go
The upstream file credits code adapted from
github.com/openshift/origin/pkg/util/jsonmerge.

Modifications for shearwater (2026): the implementation uses Shearwater's
type-tag schema and map/list helpers, rejects unknown directives, and returns
errors for inputs that panic in the reference implementation. The detailed
behavior and deliberate differences are described below. See NOTICE and
THIRD_PARTY_NOTICES.md for the original license and attribution scope.
*/

package patch

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Strategic merge patch (design 3.6). This follows what Kubernetes does in
// apimachinery's strategicpatch (mergeMap with MergeParallelList and
// IgnoreUnmatchedNulls, the settings the API server uses), step for step, so
// that the patches kubectl computes against that behaviour land the same way.
// testdata/smp.json holds the cases; their expected results were produced by
// Kubernetes itself.
//
// The schema (which lists merge by which key) comes from struct tags on the
// target type, with the same names Kubernetes uses:
//
//	Containers []Container `json:"containers" patchStrategy:"merge" patchMergeKey:"name"`
//
// Two deliberate differences: a "$..." key that is not a known directive is
// refused instead of being stored as a field, and inputs that make the
// Kubernetes code panic return an error.

const (
	directiveMarker       = "$patch"
	retainKeysDirective   = "$retainKeys"
	setElementOrderPrefix = "$setElementOrder"
	deletePrimitivePrefix = "$deleteFromPrimitiveList"
	deleteDirective       = "delete"
	replaceDirective      = "replace"
	mergeDirective        = "merge"
	retainKeysStrategy    = "retainKeys"
)

// Strategic applies patch to doc. t is the Go type the document decodes into.
// Neither argument is modified.
func Strategic(doc, patch map[string]any, t reflect.Type) (map[string]any, error) {
	return mergeMap(Clone(doc).(map[string]any), Clone(patch).(map[string]any), schema{t})
}

// ---- schema from struct tags ----

type schema struct{ t reflect.Type } // t == nil: no type information (free-form JSON)

type fieldMeta struct {
	t          reflect.Type
	strategies []string
	mergeKey   string
}

var (
	metaCache       sync.Map // reflect.Type -> map[string]fieldMeta
	unmarshalerType = reflect.TypeFor[json.Unmarshaler]()
)

func structFields(t reflect.Type) map[string]fieldMeta {
	if m, ok := metaCache.Load(t); ok {
		return m.(map[string]fieldMeta)
	}
	m := map[string]fieldMeta{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || !f.IsExported() {
			continue
		}
		if f.Anonymous && name == "" {
			for k, v := range structFields(f.Type) {
				m[k] = v
			}
			continue
		}
		if name == "" {
			name = f.Name
		}
		fm := fieldMeta{t: f.Type, mergeKey: f.Tag.Get("patchMergeKey")}
		if s := f.Tag.Get("patchStrategy"); s != "" {
			fm.strategies = strings.Split(s, ",")
		}
		m[name] = fm
	}
	metaCache.Store(t, m)
	return m
}

func (s schema) name() string {
	if s.t == nil {
		return "free-form object"
	}
	return s.t.String()
}

// field looks up the member key of an object of this type.
func (s schema) field(key string) (schema, fieldMeta, error) {
	t := s.t
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || reflect.PointerTo(t).Implements(unmarshalerType) {
		return schema{}, fieldMeta{}, nil // stored as-is: objects merge, lists replace
	}
	switch t.Kind() {
	case reflect.Map:
		return schema{t.Elem()}, fieldMeta{}, nil
	case reflect.Struct:
		fm, ok := structFields(t)[key]
		if !ok {
			return schema{}, fieldMeta{}, fmt.Errorf("%s has no field %q", t.Name(), key)
		}
		return schema{fm.t}, fm, nil
	}
	return schema{}, fieldMeta{}, nil
}

// list looks up the member key, which must be a list, and returns the schema of its elements.
func (s schema) list(key string) (schema, fieldMeta, error) {
	sub, fm, err := s.field(key)
	if err != nil {
		return schema{}, fm, err
	}
	t := sub.t
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || reflect.PointerTo(t).Implements(unmarshalerType) {
		return schema{}, fieldMeta{}, nil
	}
	if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
		return schema{}, fm, fmt.Errorf("field %q of %s is not a list", key, s.name())
	}
	return schema{t.Elem()}, fm, nil
}

// splitStrategy separates "retainKeys" from the list strategy ("merge", "replace" or "").
func splitStrategy(strategies []string) (string, error) {
	rest := slices.DeleteFunc(slices.Clone(strategies), func(s string) bool { return s == retainKeysStrategy })
	switch {
	case len(strategies) > 2 || len(rest) > 1:
		return "", fmt.Errorf("unexpected patch strategy: %v", strategies)
	case len(rest) == 1:
		return rest[0], nil
	}
	return "", nil
}

// ---- values ----

func kindOf(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "list"
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "bool"
	}
	return "number"
}

// same compares two scalars. Objects and lists are never equal to anything.
func same(a, b any) bool {
	switch kindOf(a) {
	case "object", "list":
		return false
	}
	switch kindOf(b) {
	case "object", "list":
		return false
	}
	return a == b
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func directiveKey(key, prefix string) (string, error) {
	p, rest, ok := strings.Cut(key, "/")
	if !ok || p != prefix || rest == "" {
		return "", fmt.Errorf("directive %q must have the form %s/FIELD", key, prefix)
	}
	return rest, nil
}

// dropNulls removes every null member, at any depth.
func dropNulls(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if e == nil {
				delete(t, k)
			} else {
				dropNulls(e)
			}
		}
	case []any:
		for _, e := range t {
			dropNulls(e)
		}
	}
}

// dropDirectives removes "$patch" elements from a value that is taken as-is
// because the document has nothing at that place yet. keep is false when v
// itself is such an element.
func dropDirectives(v any) (out any, keep bool) {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t[directiveMarker]; ok {
			return nil, false
		}
		for k, e := range t {
			if ne, keep := dropDirectives(e); keep {
				t[k] = ne
			} else {
				delete(t, k)
			}
		}
		return t, true
	case []any:
		var tmp []any
		for _, e := range t {
			if ne, keep := dropDirectives(e); keep {
				tmp = append(tmp, ne)
			}
		}
		return tmp, true
	}
	return v, true
}

// ---- merge ----

func mergeMap(original, patch map[string]any, sch schema) (map[string]any, error) {
	if d, ok := patch[directiveMarker]; ok {
		switch d {
		case replaceDirective:
			delete(patch, directiveMarker)
			return patch, nil
		case deleteDirective:
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("unknown $patch value %q in an object; use \"replace\" or \"delete\"", fmt.Sprint(d))
	}
	if original == nil {
		original = map[string]any{}
	}
	for k := range patch {
		if strings.HasPrefix(k, "$") && k != retainKeysDirective &&
			!strings.HasPrefix(k, setElementOrderPrefix+"/") && !strings.HasPrefix(k, deletePrimitivePrefix+"/") {
			return nil, fmt.Errorf("unknown directive %q; known directives are $patch, $retainKeys, $setElementOrder/FIELD and $deleteFromPrimitiveList/FIELD", k)
		}
	}
	if err := applyRetainKeys(original, patch); err != nil {
		return nil, err
	}
	if err := applyElementOrder(original, patch, sch); err != nil {
		return nil, err
	}

	for _, k := range sortedKeys(patch) {
		patchV := patch[k]
		isDeleteList := false
		if strings.HasPrefix(k, deletePrimitivePrefix) {
			var err error
			if k, err = directiveKey(k, deletePrimitivePrefix); err != nil {
				return nil, err
			}
			isDeleteList = true
		}
		if patchV == nil {
			delete(original, k)
			continue
		}
		origV, ok := original[k]
		if !ok || kindOf(origV) != kindOf(patchV) {
			// Nothing to merge with: the patch value is the new value.
			if !isDeleteList {
				dropNulls(patchV)
				if v, keep := dropDirectives(patchV); keep {
					original[k] = v
				} else {
					delete(original, k)
				}
			}
			continue
		}
		switch ov := origV.(type) {
		case map[string]any:
			sub, fm, err := sch.field(k)
			if err != nil {
				return nil, err
			}
			strategy, err := splitStrategy(fm.strategies)
			if err != nil {
				return nil, err
			}
			if strategy == replaceDirective {
				original[k] = patchV
				continue
			}
			merged, err := mergeMap(ov, patchV.(map[string]any), sub)
			if err != nil {
				return nil, err
			}
			original[k] = merged
		case []any:
			sub, fm, err := sch.list(k)
			if err != nil {
				return nil, err
			}
			strategy, err := splitStrategy(fm.strategies)
			if err != nil {
				return nil, err
			}
			merged, err := mergeListField(ov, patchV.([]any), sub, strategy, fm.mergeKey, isDeleteList)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", k, err)
			}
			original[k] = merged
		default:
			original[k] = patchV
		}
	}
	return original, nil
}

// applyRetainKeys handles $retainKeys: every key in the patch must be listed,
// and keys of the original that are not listed are removed.
func applyRetainKeys(original, patch map[string]any) error {
	v, ok := patch[retainKeysDirective]
	if !ok {
		return nil
	}
	delete(patch, retainKeysDirective)
	list, ok := v.([]any)
	if !ok {
		return fmt.Errorf("$retainKeys must be a list of field names")
	}
	keep := map[string]bool{}
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return fmt.Errorf("$retainKeys must be a list of field names")
		}
		keep[s] = true
	}
	for k, v := range patch {
		if v == nil || strings.HasPrefix(k, deletePrimitivePrefix) || strings.HasPrefix(k, setElementOrderPrefix) {
			continue
		}
		if !keep[k] {
			return fmt.Errorf("$retainKeys %v does not list %q, which the patch sets", list, k)
		}
	}
	for k := range original {
		if !keep[k] {
			delete(original, k)
		}
	}
	return nil
}

// applyElementOrder handles every $setElementOrder/FIELD together with the
// patch list of the same field, and removes both from the patch.
func applyElementOrder(original, patch map[string]any, sch schema) error {
	for _, key := range sortedKeys(patch) {
		if !strings.HasPrefix(key, setElementOrderPrefix) {
			continue
		}
		order, ok := patch[key].([]any)
		if !ok {
			return fmt.Errorf("%s must be a list", key)
		}
		delete(patch, key)
		field, err := directiveKey(key, setElementOrderPrefix)
		if err != nil {
			return err
		}
		var origList, patchList []any
		origV, foundOrig := original[field]
		patchV, foundPatch := patch[field]
		if foundOrig {
			if origList, ok = origV.([]any); !ok {
				return fmt.Errorf("%s: %q is not a list in the object", key, field)
			}
		}
		if foundPatch {
			if patchList, ok = patchV.([]any); !ok {
				return fmt.Errorf("%s: %q is not a list in the patch", key, field)
			}
		}
		sub, fm, err := sch.list(field)
		if err != nil {
			return err
		}
		strategy, err := splitStrategy(fm.strategies)
		if err != nil {
			return err
		}
		for _, l := range [][]any{order, origList, patchList} {
			for _, e := range l {
				if isObj := kindOf(e) == "object"; isObj != (fm.mergeKey != "") {
					return fmt.Errorf("%s: %q is not a list that can be ordered element by element (it is replaced as a whole)", key, field)
				}
			}
		}
		if err := checkPatchOrder(patchList, order, fm.mergeKey); err != nil {
			return fmt.Errorf("%s: %v", key, err)
		}

		var merged []any
		switch {
		case foundOrig && !foundPatch:
			merged = origList
		case !foundOrig && foundPatch:
			v, _ := dropDirectives(patchList)
			merged, _ = v.([]any)
		case foundOrig && foundPatch:
			if merged, err = mergeListField(origList, patchList, sub, strategy, fm.mergeKey, false); err != nil {
				return fmt.Errorf("%s: %v", field, err)
			}
		default:
			continue
		}
		patchItems, serverOnly, err := partition(merged, order, fm.mergeKey)
		if err != nil {
			return err
		}
		both, err := normalizeOrder(patchItems, serverOnly, order, origList, fm.mergeKey)
		if err != nil {
			return err
		}
		original[field] = both
		delete(patch, field)
	}
	return nil
}

// checkPatchOrder: the elements of the patch list (deletions aside) must appear
// in the order list, in the same relative order.
func checkPatchOrder(patchList, order []any, mergeKey string) error {
	if len(order) == 0 || len(patchList) == 0 {
		return nil
	}
	var rest []any
	for _, e := range patchList {
		if m, ok := e.(map[string]any); ok {
			if _, has := m[directiveMarker]; has {
				continue
			}
		}
		rest = append(rest, e)
	}
	pi, oi := 0, 0
	for pi < len(rest) && oi < len(order) {
		if keyOf(rest[pi], mergeKey) != nil && same(keyOf(rest[pi], mergeKey), keyOf(order[oi], mergeKey)) {
			pi++
		}
		oi++
	}
	if pi < len(rest) {
		return fmt.Errorf("the patch list has an element (%v) that the order list does not name, or names in another order", keyOf(rest[pi], mergeKey))
	}
	return nil
}

// keyOf returns what identifies a list element: its merge key value, or itself for a scalar.
func keyOf(e any, mergeKey string) any {
	if m, ok := e.(map[string]any); ok {
		return m[mergeKey]
	}
	if mergeKey != "" {
		return nil
	}
	return e
}

func mergeListField(original, patch []any, sub schema, strategy, mergeKey string, isDeleteList bool) ([]any, error) {
	if strategy != mergeDirective && !isDeleteList {
		return patch, nil // the list is replaced as a whole
	}
	if len(original) == 0 && len(patch) == 0 {
		return original, nil
	}
	elem := ""
	for _, l := range [][]any{original, patch} {
		for _, e := range l {
			k := kindOf(e)
			switch {
			case k == "list":
				return nil, fmt.Errorf("lists of lists cannot be merged")
			case elem == "":
				elem = k
			case elem != k:
				return nil, fmt.Errorf("list elements are not all of the same type")
			}
		}
	}

	var merged []any
	if elem != "object" {
		if isDeleteList {
			return slices.DeleteFunc(slices.Clone(original), func(v any) bool {
				return slices.ContainsFunc(patch, func(d any) bool { return same(d, v) })
			}), nil
		}
		for _, v := range append(slices.Clone(original), patch...) { // union, first occurrence wins
			if !slices.ContainsFunc(merged, func(m any) bool { return same(m, v) }) {
				merged = append(merged, v)
			}
		}
	} else {
		if isDeleteList {
			return nil, fmt.Errorf("$deleteFromPrimitiveList works on lists of plain values, not objects")
		}
		if mergeKey == "" {
			return nil, fmt.Errorf("cannot merge a list of objects that has no merge key")
		}
		var err error
		if original, patch, err = applyListDirectives(original, patch, mergeKey); err != nil {
			return nil, err
		}
		merged = original
		for _, e := range patch {
			pm := e.(map[string]any)
			kv, ok := pm[mergeKey]
			if !ok {
				return nil, fmt.Errorf("list element %v does not have the merge key %q", compact(pm), mergeKey)
			}
			if i := indexOf(merged, kv, mergeKey); i >= 0 {
				m, err := mergeMap(merged[i].(map[string]any), pm, sub)
				if err != nil {
					return nil, err
				}
				merged[i] = m
			} else {
				merged = append(merged, e)
			}
		}
	}
	patchItems, serverOnly, err := partition(merged, patch, mergeKey)
	if err != nil {
		return nil, err
	}
	return normalizeOrder(patchItems, serverOnly, patch, original, mergeKey)
}

// applyListDirectives carries out the "$patch" elements of a patch list:
// delete removes every element with that key, replace makes the rest of the
// patch the whole list. It returns the original and the patch without them.
func applyListDirectives(original, patch []any, mergeKey string) ([]any, []any, error) {
	plain := []any{}
	replace := false
	for _, e := range patch {
		pm := e.(map[string]any)
		d, ok := pm[directiveMarker]
		if !ok {
			plain = append(plain, e)
			continue
		}
		switch d {
		case deleteDirective:
			kv, ok := pm[mergeKey]
			if !ok {
				return nil, nil, fmt.Errorf("list element %v does not have the merge key %q", compact(pm), mergeKey)
			}
			// Shift in place without clearing the tail, as Kubernetes does: the
			// caller's longer view of this list is later used as the order to keep.
			for i := indexOf(original, kv, mergeKey); i >= 0; i = indexOf(original, kv, mergeKey) {
				original = append(original[:i], original[i+1:]...)
			}
		case replaceDirective:
			replace = true
		default:
			return nil, nil, fmt.Errorf("unknown $patch value %q in a list element; use \"delete\" or \"replace\"", fmt.Sprint(d))
		}
	}
	if replace {
		return plain, nil, nil
	}
	return original, plain, nil
}

func indexOf(list []any, key any, mergeKey string) int {
	if key == nil {
		return -1
	}
	for i, e := range list {
		if same(keyOf(e, mergeKey), key) {
			return i
		}
	}
	return -1
}

// partition splits list into the elements that by names, and the rest.
func partition(list, by []any, mergeKey string) (named, rest []any, err error) {
	for _, e := range list {
		k := keyOf(e, mergeKey)
		if mergeKey != "" {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("list element %v is not an object", e)
			}
			if _, ok := m[mergeKey]; !ok {
				return nil, nil, fmt.Errorf("list element %v does not have the merge key %q", compact(m), mergeKey)
			}
		} else if kindOf(e) == "object" || kindOf(e) == "list" {
			return nil, nil, fmt.Errorf("list element %v is not a plain value", e)
		}
		if indexOf(by, k, mergeKey) >= 0 {
			named = append(named, e)
		} else {
			rest = append(rest, e)
		}
	}
	return named, rest, nil
}

// normalizeOrder puts the patch's elements in the patch's order, the others in
// the object's order, and interleaves the two by the object's order as far as
// that is known. The sort and the interleaving are the ones Kubernetes uses,
// including their behaviour for elements that have no known position.
func normalizeOrder(patchItems, serverOnly, patchOrder, serverOrder []any, mergeKey string) ([]any, error) {
	patchItems, err := sortBy(patchItems, patchOrder, mergeKey)
	if err != nil {
		return nil, err
	}
	serverOnly, err = sortBy(serverOnly, serverOrder, mergeKey)
	if err != nil {
		return nil, err
	}
	left, right := serverOnly, patchItems
	out := make([]any, 0, len(left)+len(right))
	i, j := 0, 0
	for i < len(left) || j < len(right) {
		switch {
		case i >= len(left):
			out = append(out, right[j])
			j++
		case j >= len(right):
			out = append(out, left[i])
			i++
		default:
			li := indexOf(serverOrder, keyOf(left[i], mergeKey), mergeKey)
			ri := indexOf(serverOrder, keyOf(right[j], mergeKey), mergeKey)
			if li >= 0 && ri >= 0 && li < ri {
				out = append(out, left[i])
				i++
			} else {
				out = append(out, right[j])
				j++
			}
		}
	}
	return out, nil
}

func sortBy(list, order []any, mergeKey string) ([]any, error) {
	var deleted []any
	if mergeKey != "" {
		for _, l := range [][]any{list, order} {
			for _, e := range l {
				m, ok := e.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("list element %v is not an object", e)
				}
				if _, ok := m[mergeKey]; !ok {
					return nil, fmt.Errorf("list element %v does not have the merge key %q", compact(m), mergeKey)
				}
			}
		}
		var kept []any
		for _, e := range list {
			if e.(map[string]any)[directiveMarker] == deleteDirective {
				deleted = append(deleted, e)
			} else {
				kept = append(kept, e)
			}
		}
		list = kept
	}
	list = slices.Clone(list)
	sort.SliceStable(list, func(i, j int) bool {
		if ii := indexOf(order, keyOf(list[i], mergeKey), mergeKey); ii >= 0 {
			if ij := indexOf(order, keyOf(list[j], mergeKey), mergeKey); ij >= 0 {
				return ii < ij
			}
		}
		return true
	})
	return append(list, deleted...), nil
}

func compact(v any) string {
	b, _ := json.Marshal(v)
	if len(b) > 120 {
		return string(b[:120]) + "..."
	}
	return string(b)
}
