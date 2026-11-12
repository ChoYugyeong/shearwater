// Package patch implements the three patch formats kubectl sends: JSON merge
// patch (RFC 7386), JSON patch (RFC 6902) and Kubernetes' strategic merge
// patch. Documents are generic JSON trees as produced by Parse: map[string]any,
// []any, string, json.Number, bool and nil.
package patch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Parse decodes one JSON value, keeping numbers as written.
func Parse(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("not valid JSON: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("data after the JSON value")
	}
	return v, nil
}

// Clone copies a JSON tree.
func Clone(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[k] = Clone(e)
		}
		return m
	case []any:
		if t == nil {
			return t
		}
		l := make([]any, len(t))
		for i, e := range t {
			l[i] = Clone(e)
		}
		return l
	}
	return v
}

// Equal compares two JSON trees. Numbers are equal when they have the same value.
func Equal(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !Equal(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		if x == y {
			return true
		}
		fx, e1 := x.Float64()
		fy, e2 := y.Float64()
		return e1 == nil && e2 == nil && fx == fy
	}
	return a == b
}

// Merge applies a JSON merge patch (RFC 7386, section 2). Neither argument is modified.
func Merge(doc, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return Clone(patch)
	}
	out := map[string]any{}
	if dm, ok := doc.(map[string]any); ok {
		for k, v := range dm {
			out[k] = Clone(v)
		}
	}
	for k, v := range pm {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = Merge(out[k], v)
		}
	}
	return out
}

// ---- JSON patch (RFC 6902) ----

type operation struct {
	Op    string           `json:"op"`
	Path  *string          `json:"path"`
	From  *string          `json:"from"`
	Value *json.RawMessage `json:"value"`
}

// JSONPatch applies a JSON patch document (an array of operations). doc is not
// modified. The first failing operation stops the patch.
func JSONPatch(doc any, patch []byte) (any, error) {
	var ops []operation
	if err := json.Unmarshal(patch, &ops); err != nil {
		return nil, fmt.Errorf("a JSON patch is an array of operations: %v", err)
	}
	doc = Clone(doc)
	for i, op := range ops {
		var err error
		if doc, err = apply(doc, op); err != nil {
			return nil, fmt.Errorf("operation %d (%s): %v", i, op.Op, err)
		}
	}
	return doc, nil
}

func apply(doc any, op operation) (any, error) {
	if op.Path == nil {
		return nil, fmt.Errorf("\"path\" is missing")
	}
	path, err := pointer(*op.Path)
	if err != nil {
		return nil, err
	}
	var value any
	switch op.Op {
	case "add", "replace", "test":
		if op.Value == nil {
			return nil, fmt.Errorf("\"value\" is missing")
		}
		if value, err = Parse(*op.Value); err != nil {
			return nil, err
		}
	}
	var from []string
	switch op.Op {
	case "move", "copy":
		if op.From == nil {
			return nil, fmt.Errorf("\"from\" is missing")
		}
		if from, err = pointer(*op.From); err != nil {
			return nil, err
		}
	}

	switch op.Op {
	case "add":
		return edit(doc, path, *op.Path, func(parent any, last string) (any, error) { return insert(parent, last, value) })
	case "remove":
		return edit(doc, path, *op.Path, remove)
	case "replace":
		if _, err := lookup(doc, path, *op.Path); err != nil {
			return nil, err
		}
		if len(path) == 0 {
			return value, nil
		}
		return edit(doc, path, *op.Path, func(parent any, last string) (any, error) {
			if l, ok := parent.([]any); ok {
				i, _ := index(last, len(l)-1)
				l[i] = value
				return l, nil
			}
			parent.(map[string]any)[last] = value
			return parent, nil
		})
	case "test":
		got, err := lookup(doc, path, *op.Path)
		if err != nil {
			return nil, err
		}
		if !Equal(got, value) {
			return nil, fmt.Errorf("the value at %q is %s, not %s", *op.Path, compact(got), compact(value))
		}
		return doc, nil
	case "move", "copy":
		v, err := lookup(doc, from, *op.From)
		if err != nil {
			return nil, err
		}
		v = Clone(v)
		if op.Op == "move" {
			if strings.HasPrefix(*op.Path+"/", *op.From+"/") && *op.Path != *op.From {
				return nil, fmt.Errorf("cannot move %q into itself", *op.From)
			}
			if doc, err = edit(doc, from, *op.From, remove); err != nil {
				return nil, err
			}
		}
		return edit(doc, path, *op.Path, func(parent any, last string) (any, error) { return insert(parent, last, v) })
	}
	return nil, fmt.Errorf("unknown operation; use add, remove, replace, move, copy or test")
}

// pointer splits a JSON pointer (RFC 6901) into its reference tokens.
func pointer(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	if p[0] != '/' {
		return nil, fmt.Errorf("path %q must start with '/'", p)
	}
	toks := strings.Split(p[1:], "/")
	for i, t := range toks {
		toks[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	return toks, nil
}

// index parses an array index that may be at most max.
func index(tok string, max int) (int, error) {
	if tok == "" || (len(tok) > 1 && tok[0] == '0') || tok[0] < '0' || tok[0] > '9' {
		return 0, fmt.Errorf("%q is not an array index", tok)
	}
	i, err := strconv.Atoi(tok)
	if err != nil {
		return 0, fmt.Errorf("%q is not an array index", tok)
	}
	if i > max {
		return 0, fmt.Errorf("index %d is out of range", i)
	}
	return i, nil
}

func child(v any, tok string) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		c, ok := t[tok]
		if !ok {
			return nil, fmt.Errorf("there is no member %q", tok)
		}
		return c, nil
	case []any:
		i, err := index(tok, len(t)-1)
		if err != nil {
			return nil, err
		}
		return t[i], nil
	}
	return nil, fmt.Errorf("%q is inside a value that is not an object or an array", tok)
}

func lookup(doc any, path []string, full string) (any, error) {
	for _, tok := range path {
		var err error
		if doc, err = child(doc, tok); err != nil {
			return nil, fmt.Errorf("path %q: %v", full, err)
		}
	}
	return doc, nil
}

// edit walks to the parent of the path's last token, lets fn change it, and
// puts the result back. With an empty path fn is called with a nil parent.
func edit(doc any, path []string, full string, fn func(parent any, last string) (any, error)) (any, error) {
	if len(path) == 0 {
		v, err := fn(nil, "")
		if err != nil {
			return nil, fmt.Errorf("path %q: %v", full, err)
		}
		return v, nil
	}
	if len(path) == 1 {
		v, err := fn(doc, path[0])
		if err != nil {
			return nil, fmt.Errorf("path %q: %v", full, err)
		}
		return v, nil
	}
	c, err := child(doc, path[0])
	if err != nil {
		return nil, fmt.Errorf("path %q: %v", full, err)
	}
	nc, err := edit(c, path[1:], full, fn)
	if err != nil {
		return nil, err
	}
	switch t := doc.(type) {
	case map[string]any:
		t[path[0]] = nc
	case []any:
		i, _ := index(path[0], len(t)-1)
		t[i] = nc
	}
	return doc, nil
}

func insert(parent any, last string, v any) (any, error) {
	switch t := parent.(type) {
	case nil:
		return v, nil // the whole document
	case map[string]any:
		t[last] = v
		return t, nil
	case []any:
		i := len(t)
		if last != "-" {
			var err error
			if i, err = index(last, len(t)); err != nil {
				return nil, err
			}
		}
		t = append(t, nil)
		copy(t[i+1:], t[i:])
		t[i] = v
		return t, nil
	}
	return nil, fmt.Errorf("cannot add %q to a value that is not an object or an array", last)
}

func remove(parent any, last string) (any, error) {
	switch t := parent.(type) {
	case nil:
		return nil, fmt.Errorf("cannot remove the whole document")
	case map[string]any:
		if _, ok := t[last]; !ok {
			return nil, fmt.Errorf("there is no member %q", last)
		}
		delete(t, last)
		return t, nil
	case []any:
		i, err := index(last, len(t)-1)
		if err != nil {
			return nil, err
		}
		return append(t[:i], t[i+1:]...), nil
	}
	return nil, fmt.Errorf("cannot remove %q from a value that is not an object or an array", last)
}
