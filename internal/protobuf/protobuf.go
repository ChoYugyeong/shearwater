/*
Copyright The Kubernetes Authors.
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

The runtime.Unknown/TypeMeta envelope field mapping is derived from:
https://github.com/kubernetes/kubernetes/blob/v1.36.6/staging/src/k8s.io/apimachinery/pkg/runtime/generated.proto

Modifications for shearwater (2026): the envelope schema is used by this
request-only wire reader and JSON conversion, together with the manually
transcribed metadata in tables.go. The reader validates the supported wire
types, fields and message depth. See NOTICE and THIRD_PARTY_NOTICES.md for
the original license and schema attribution.
*/

// Package protobuf reads the Protobuf request bodies that `kubectl create
// <kind>` sends (design 3.6, decision 5). It only reads; responses are JSON.
//
// A body is turned into the JSON the same object would have had, so that
// everything after it (unknown-field rules, defaults, validation, storage) is
// the one code path that JSON requests take. Nothing here imports k8s.io: the
// field numbers in tables.go are copied from the Kubernetes .proto files.
//
// What "the same JSON" means: the Kubernetes client writes every non-pointer
// field of its Go types, set or not (name="", paused=false, an empty status).
// Such a field is left out of the JSON when it holds the zero value, exactly as
// the Go type's omitempty would. A field that is a pointer in the Go type is
// kept whenever it is on the wire, so replicas=0 or an empty emptyDir survive.
// A field number that is not in the tables is an error, never skipped: its
// value cannot be judged without knowing what it is.
package protobuf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"
)

// ContentType is the media type of these bodies.
const ContentType = "application/vnd.kubernetes.protobuf"

var magic = []byte{'k', '8', 's', 0}

const maxDepth = 24

// Decode reads a body: the four magic bytes followed by a runtime.Unknown
// message that names the type and carries the object. It returns the object as
// JSON, with apiVersion and kind set from the envelope.
func Decode(body []byte) (apiVersion, kind string, js []byte, err error) {
	if !bytes.HasPrefix(body, magic) {
		return "", "", nil, fmt.Errorf("the body does not start with the Kubernetes protobuf marker \"k8s\\x00\"")
	}
	var raw []byte
	var encoding string
	r := reader{b: body[len(magic):]}
	for !r.done() {
		num, wt, err := r.tag()
		if err != nil {
			return "", "", nil, fmt.Errorf("envelope: %v", err)
		}
		if wt != wireBytes || num < 1 || num > 4 {
			return "", "", nil, fmt.Errorf("envelope: unexpected field %d (wire type %d)", num, wt)
		}
		p, err := r.bytes()
		if err != nil {
			return "", "", nil, fmt.Errorf("envelope: %v", err)
		}
		switch num {
		case 1: // TypeMeta: 1 apiVersion, 2 kind
			tr := reader{b: p}
			for !tr.done() {
				n, wt, err := tr.tag()
				if err != nil || wt != wireBytes {
					return "", "", nil, fmt.Errorf("envelope: the type is not readable")
				}
				v, err := tr.bytes()
				if err != nil {
					return "", "", nil, fmt.Errorf("envelope: %v", err)
				}
				switch n {
				case 1:
					apiVersion = string(v)
				case 2:
					kind = string(v)
				}
			}
		case 2:
			raw = p
		case 3:
			encoding = string(p)
		case 4: // contentType: usually empty and never needed
		}
	}
	if encoding != "" {
		return "", "", nil, fmt.Errorf("envelope: content encoding %q is not supported", encoding)
	}
	root, ok := roots[apiVersion+" "+kind]
	if !ok {
		return apiVersion, kind, nil, fmt.Errorf("protobuf bodies are read for v1 Pod, Service, Namespace and apps/v1 Deployment only, not %s %s", apiVersion, kind)
	}
	out := map[string]any{}
	if err := decodeMessage(root, raw, out, 0); err != nil {
		return apiVersion, kind, nil, err
	}
	out["apiVersion"], out["kind"] = apiVersion, kind
	js, err = json.Marshal(out)
	return apiVersion, kind, js, err
}

func decodeMessage(m *message, data []byte, out map[string]any, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%s: nested too deeply", m.name)
	}
	r := reader{b: data}
	for !r.done() {
		num, wt, err := r.tag()
		if err != nil {
			return fmt.Errorf("%s: %v", m.name, err)
		}
		f, ok := m.fields[num]
		if !ok {
			return fmt.Errorf("%s: field number %d is not in Shearwater's tables (a kubectl newer than the tables may send it)", m.name, num)
		}
		where := m.name + "." + f.name
		if f.kind == tSkip {
			if err := r.skip(wt); err != nil {
				return fmt.Errorf("%s: %v", where, err)
			}
			continue
		}
		if want := f.kind.wire(); wt != want {
			return fmt.Errorf("%s: wire type %d, expected %d", where, wt, want)
		}

		var v any    // the JSON value
		zero := true // whether v is the Go zero value
		switch f.kind {
		case tBool, tInt:
			u, err := r.varint()
			if err != nil {
				return fmt.Errorf("%s: %v", where, err)
			}
			if f.kind == tBool {
				v, zero = u != 0, u == 0
			} else {
				v, zero = json.Number(strconv.FormatInt(int64(u), 10)), u == 0
			}
		default:
			p, err := r.bytes()
			if err != nil {
				return fmt.Errorf("%s: %v", where, err)
			}
			switch f.kind {
			case tString:
				if !utf8.Valid(p) {
					return fmt.Errorf("%s: not valid UTF-8", where)
				}
				v, zero = string(p), len(p) == 0
			case tStrMap, tQtyMap:
				k, val, err := mapEntry(p, f.kind == tQtyMap)
				if err != nil {
					return fmt.Errorf("%s: %v", where, err)
				}
				mm, _ := out[f.name].(map[string]any)
				if mm == nil {
					mm = map[string]any{}
					out[f.name] = mm
				}
				mm[k] = val
				continue
			case tTime:
				if v, zero, err = timeValue(p); err != nil {
					return fmt.Errorf("%s: %v", where, err)
				}
			case tIntOrStr:
				if v, zero, err = intOrString(p); err != nil {
					return fmt.Errorf("%s: %v", where, err)
				}
			case tQuantity:
				s, err := quantity(p)
				if err != nil {
					return fmt.Errorf("%s: %v", where, err)
				}
				v, zero = s, false
			case tInline:
				if err := decodeMessage(f.msg, p, out, depth+1); err != nil {
					return err
				}
				continue
			case tMsg:
				if f.msg == nil {
					if len(p) == 0 && !f.ptr && !f.rep {
						continue // an unset struct, written because the Go field is not a pointer
					}
					return fmt.Errorf("%s is set, and Shearwater does not read that field from a protobuf body", where)
				}
				sub := map[string]any{}
				if prev, ok := out[f.name].(map[string]any); ok && !f.rep {
					sub = prev // a message that appears twice is merged
				}
				if err := decodeMessage(f.msg, p, sub, depth+1); err != nil {
					return err
				}
				v, zero = sub, len(sub) == 0
			}
		}

		switch {
		case f.rep:
			l, _ := out[f.name].([]any)
			out[f.name] = append(l, v)
		case zero && !f.ptr:
			delete(out, f.name) // the last occurrence wins, and it says "not set"
		default:
			out[f.name] = v
		}
	}
	return nil
}

// mapEntry reads one map entry: key = 1, value = 2.
func mapEntry(p []byte, quantityValue bool) (key string, value any, err error) {
	r := reader{b: p}
	value = ""
	for !r.done() {
		num, wt, err := r.tag()
		if err != nil {
			return "", nil, err
		}
		if wt != wireBytes || (num != 1 && num != 2) {
			return "", nil, fmt.Errorf("map entry has unexpected field %d", num)
		}
		b, err := r.bytes()
		if err != nil {
			return "", nil, err
		}
		switch {
		case num == 1:
			key = string(b)
		case quantityValue:
			if value, err = quantity(b); err != nil {
				return "", nil, err
			}
		default:
			value = string(b)
		}
	}
	return key, value, nil
}

// quantity: resource.Quantity is a message whose field 1 is the text form ("500m").
func quantity(p []byte) (string, error) {
	r := reader{b: p}
	s := ""
	for !r.done() {
		num, wt, err := r.tag()
		if err != nil {
			return "", err
		}
		if num != 1 || wt != wireBytes {
			return "", fmt.Errorf("quantity has unexpected field %d", num)
		}
		b, err := r.bytes()
		if err != nil {
			return "", err
		}
		s = string(b)
	}
	return s, nil
}

// timeValue: meta/v1 Time is seconds = 1, nanos = 2. An empty message is "no
// time", not 1970.
func timeValue(p []byte) (any, bool, error) {
	if len(p) == 0 {
		return nil, true, nil
	}
	var sec, nanos int64
	r := reader{b: p}
	for !r.done() {
		num, wt, err := r.tag()
		if err != nil {
			return nil, false, err
		}
		if wt != wireVarint || (num != 1 && num != 2) {
			return nil, false, fmt.Errorf("time has unexpected field %d", num)
		}
		u, err := r.varint()
		if err != nil {
			return nil, false, err
		}
		if num == 1 {
			sec = int64(u)
		} else {
			nanos = int64(int32(u))
		}
	}
	return time.Unix(sec, nanos).UTC().Format(time.RFC3339), false, nil
}

// intOrString: type = 1 (0 integer, 1 string), intVal = 2, strVal = 3.
func intOrString(p []byte) (any, bool, error) {
	var typ, iv int64
	var sv string
	r := reader{b: p}
	for !r.done() {
		num, wt, err := r.tag()
		if err != nil {
			return nil, false, err
		}
		switch {
		case num == 1 && wt == wireVarint, num == 2 && wt == wireVarint:
			u, err := r.varint()
			if err != nil {
				return nil, false, err
			}
			if num == 1 {
				typ = int64(u)
			} else {
				iv = int64(int32(u))
			}
		case num == 3 && wt == wireBytes:
			b, err := r.bytes()
			if err != nil {
				return nil, false, err
			}
			sv = string(b)
		default:
			return nil, false, fmt.Errorf("int-or-string has unexpected field %d", num)
		}
	}
	switch typ {
	case 0:
		return json.Number(strconv.FormatInt(iv, 10)), iv == 0, nil
	case 1:
		return sv, false, nil
	}
	return nil, false, fmt.Errorf("int-or-string has unknown type %d", typ)
}

// ---- wire format ----

const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

type reader struct {
	b   []byte
	pos int
}

func (r *reader) done() bool { return r.pos >= len(r.b) }

func (r *reader) varint() (uint64, error) {
	var v uint64
	for i := 0; i < 10; i++ {
		if r.pos >= len(r.b) {
			return 0, fmt.Errorf("the data ends inside a number")
		}
		c := r.b[r.pos]
		r.pos++
		if i == 9 && c > 1 {
			return 0, fmt.Errorf("a number does not fit in 64 bits")
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, nil
		}
	}
	return 0, fmt.Errorf("a number is longer than 10 bytes")
}

func (r *reader) tag() (num, wireType int, err error) {
	v, err := r.varint()
	if err != nil {
		return 0, 0, err
	}
	if v>>3 == 0 || v>>3 > 1<<29-1 {
		return 0, 0, fmt.Errorf("field number %d is out of range", v>>3)
	}
	return int(v >> 3), int(v & 7), nil
}

func (r *reader) bytes() ([]byte, error) {
	n, err := r.varint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(r.b)-r.pos) {
		return nil, fmt.Errorf("a length (%d) runs past the end of the data", n)
	}
	p := r.b[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return p, nil
}

func (r *reader) skip(wireType int) error {
	switch wireType {
	case wireVarint:
		_, err := r.varint()
		return err
	case wireBytes:
		_, err := r.bytes()
		return err
	case wireFixed64, wireFixed32:
		n := 8
		if wireType == wireFixed32 {
			n = 4
		}
		if len(r.b)-r.pos < n {
			return fmt.Errorf("the data ends inside a fixed-size number")
		}
		r.pos += n
		return nil
	}
	return fmt.Errorf("wire type %d is not supported", wireType)
}
