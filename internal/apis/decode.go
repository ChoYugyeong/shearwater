package apis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
)

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// Decode parses a JSON object into obj and reports, by path, every member the
// type does not declare ("spec.containers[0].foo"). Those members and all null
// members are removed before decoding, so what ends up in obj is exactly what
// the type knows. Top-level members named in drop are removed silently.
func Decode(body []byte, obj any, drop ...string) (unknown []string, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("body is not valid JSON: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("body has data after the JSON object")
	}
	top, ok := tree.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("body must be a JSON object")
	}
	for _, k := range drop {
		delete(top, k)
	}
	prune(reflect.TypeOf(obj), tree, "", &unknown)
	slices.Sort(unknown)
	clean, err := json.Marshal(tree)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(clean, obj); err != nil {
		return unknown, fmt.Errorf("%s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return unknown, nil
}

// DuplicateFields returns the path of every object member that appears more
// than once in a JSON document ("spec.replicas", "spec.containers[0].name",
// "metadata.labels.app"). Decode cannot see these: it reads objects into maps,
// where a repeated key silently keeps the last value. The scan knows nothing
// about types, so it also looks inside values that are stored as they are
// (affinity, probes). Top-level members named in drop are not looked into,
// and a repeat of such a member itself is not reported. A document that is not
// valid JSON yields what was found before the error; Decode reports the error.
func DuplicateFields(body []byte, drop ...string) []string {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var dups []string
	var walk func(path string, quiet bool) error
	walk = func(path string, quiet bool) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				key, _ := kt.(string)
				child, q := key, quiet
				if path != "" {
					child = path + "." + key
				} else if slices.Contains(drop, key) {
					q = true
				}
				if seen[key] && !q {
					dups = append(dups, child)
				}
				seen[key] = true
				if err := walk(child, q); err != nil {
					return err
				}
			}
			_, err = dec.Token()
		case json.Delim('['):
			for i := 0; dec.More(); i++ {
				if err := walk(fmt.Sprintf("%s[%d]", path, i), quiet); err != nil {
					return err
				}
			}
			_, err = dec.Token()
		}
		return err
	}
	walk("", false)
	slices.Sort(dups)
	return slices.Compact(dups)
}

var fieldCache sync.Map // reflect.Type -> map[string]reflect.Type

func jsonFields(t reflect.Type) map[string]reflect.Type {
	if m, ok := fieldCache.Load(t); ok {
		return m.(map[string]reflect.Type)
	}
	m := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "-" || !f.IsExported() {
			continue
		}
		if f.Anonymous && tag == "" {
			for k, v := range jsonFields(f.Type) {
				m[k] = v
			}
			continue
		}
		if tag == "" {
			tag = f.Name
		}
		m[tag] = f.Type
	}
	fieldCache.Store(t, m)
	return m
}

func prune(t reflect.Type, v any, path string, unknown *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(unmarshalerType) {
		return // the type parses itself (RawMessage, Time, Quantity, IntOrString)
	}
	switch vv := v.(type) {
	case map[string]any:
		switch t.Kind() {
		case reflect.Struct:
			fields := jsonFields(t)
			for k, val := range vv {
				child := k
				if path != "" {
					child = path + "." + k
				}
				ft, ok := fields[k]
				switch {
				case !ok:
					delete(vv, k)
					*unknown = append(*unknown, child)
				case val == nil:
					delete(vv, k)
				default:
					prune(ft, val, child, unknown)
				}
			}
		case reflect.Map:
			for k, val := range vv {
				prune(t.Elem(), val, path+"."+k, unknown)
			}
		}
	case []any:
		if t.Kind() == reflect.Slice {
			for i, el := range vv {
				prune(t.Elem(), el, fmt.Sprintf("%s[%d]", path, i), unknown)
			}
		}
	}
}

// rawEmpty reports whether a stored-as-is JSON value carries nothing.
func rawEmpty(r json.RawMessage) bool {
	switch string(bytes.TrimSpace(r)) {
	case "", "null", "[]", "{}":
		return true
	}
	return false
}
