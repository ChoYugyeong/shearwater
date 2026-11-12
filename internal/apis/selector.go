package apis

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ---- names and labels ----

func isLowerAlnum(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }
func isAlnum(c byte) bool      { return isLowerAlnum(c) || c >= 'A' && c <= 'Z' }

// IsDNS1123Label: lowercase letters, digits and '-', at most 63 characters.
func IsDNS1123Label(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if isLowerAlnum(s[i]) || s[i] == '-' && i > 0 && i < len(s)-1 {
			continue
		}
		return false
	}
	return true
}

// IsDNS1035Label is a DNS-1123 label that starts with a letter (Service names).
func IsDNS1035Label(s string) bool {
	return IsDNS1123Label(s) && s[0] >= 'a' && s[0] <= 'z'
}

// IsDNS1123Subdomain: DNS-1123 labels joined by dots, at most 253 characters.
func IsDNS1123Subdomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for part := range strings.SplitSeq(s, ".") {
		if !IsDNS1123Label(part) {
			return false
		}
	}
	return true
}

func isNamePart(s string, allowEmpty bool) bool {
	if s == "" {
		return allowEmpty
	}
	if len(s) > 63 || !isAlnum(s[0]) || !isAlnum(s[len(s)-1]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isAlnum(s[i]) && s[i] != '-' && s[i] != '_' && s[i] != '.' {
			return false
		}
	}
	return true
}

// IsQualifiedName checks a label or annotation key: [prefix/]name.
func IsQualifiedName(s string) bool {
	if prefix, name, ok := strings.Cut(s, "/"); ok {
		return IsDNS1123Subdomain(prefix) && isNamePart(name, false)
	}
	return isNamePart(s, false)
}

func IsLabelValue(s string) bool { return isNamePart(s, true) }

// ---- label selectors ----

const (
	OpIn           = "In"
	OpNotIn        = "NotIn"
	OpExists       = "Exists"
	OpDoesNotExist = "DoesNotExist"
)

type Requirement struct {
	Key    string
	Op     string
	Values []string
}

// Selector is a list of requirements that must all hold. The zero value
// matches everything.
type Selector struct {
	Reqs    []Requirement
	Nothing bool // matches no object (a nil LabelSelector)
}

func (s Selector) Empty() bool { return len(s.Reqs) == 0 && !s.Nothing }

func (s Selector) Matches(labels map[string]string) bool {
	if s.Nothing {
		return false
	}
	for _, r := range s.Reqs {
		v, has := labels[r.Key]
		switch r.Op {
		case OpIn:
			if !has || !slices.Contains(r.Values, v) {
				return false
			}
		case OpNotIn:
			if has && slices.Contains(r.Values, v) {
				return false
			}
		case OpExists:
			if !has {
				return false
			}
		case OpDoesNotExist:
			if has {
				return false
			}
		}
	}
	return true
}

func checkRequirement(r Requirement) error {
	if !IsQualifiedName(r.Key) {
		return fmt.Errorf("label key %q is not valid", r.Key)
	}
	for _, v := range r.Values {
		if !IsLabelValue(v) {
			return fmt.Errorf("label value %q is not valid", v)
		}
	}
	return nil
}

// SelectorFrom converts the object form (matchLabels, matchExpressions).
// A nil selector matches nothing; an empty one matches everything.
func SelectorFrom(ls *LabelSelector) (Selector, error) {
	if ls == nil {
		return Selector{Nothing: true}, nil
	}
	var s Selector
	keys := make([]string, 0, len(ls.MatchLabels))
	for k := range ls.MatchLabels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		s.Reqs = append(s.Reqs, Requirement{Key: k, Op: OpIn, Values: []string{ls.MatchLabels[k]}})
	}
	for _, e := range ls.MatchExpressions {
		switch e.Operator {
		case OpIn, OpNotIn:
			if len(e.Values) == 0 {
				return s, fmt.Errorf("operator %s on key %q needs at least one value", e.Operator, e.Key)
			}
		case OpExists, OpDoesNotExist:
			if len(e.Values) != 0 {
				return s, fmt.Errorf("operator %s on key %q takes no values", e.Operator, e.Key)
			}
		default:
			return s, fmt.Errorf("operator %q is not one of In, NotIn, Exists, DoesNotExist", e.Operator)
		}
		s.Reqs = append(s.Reqs, Requirement{Key: e.Key, Op: e.Operator, Values: e.Values})
	}
	for _, r := range s.Reqs {
		if err := checkRequirement(r); err != nil {
			return s, err
		}
	}
	return s, nil
}

func isSelectorWordChar(c byte) bool {
	return isAlnum(c) || c == '-' || c == '_' || c == '.' || c == '/'
}

// ParseSelector parses the query form:
//
//	a=b, a==b, a!=b, a in (x,y), a notin (x,y), a, !a
func ParseSelector(in string) (Selector, error) {
	var s Selector
	i := 0
	skip := func() {
		for i < len(in) && (in[i] == ' ' || in[i] == '\t') {
			i++
		}
	}
	word := func() string {
		start := i
		for i < len(in) && isSelectorWordChar(in[i]) {
			i++
		}
		return in[start:i]
	}
	fail := func(what string) (Selector, error) {
		return Selector{}, fmt.Errorf("label selector %q: %s at position %d", in, what, i)
	}
	skip()
	if i == len(in) {
		return s, nil
	}
	for {
		skip()
		var r Requirement
		if i < len(in) && in[i] == '!' {
			i++
			skip()
			r = Requirement{Key: word(), Op: OpDoesNotExist}
		} else {
			r.Key = word()
			skip()
			switch {
			case i == len(in) || in[i] == ',':
				r.Op = OpExists
			case strings.HasPrefix(in[i:], "!="):
				i += 2
				skip()
				r.Op, r.Values = OpNotIn, []string{word()}
			case in[i] == '=':
				i++
				if i < len(in) && in[i] == '=' {
					i++
				}
				skip()
				r.Op, r.Values = OpIn, []string{word()}
			default:
				switch op := word(); op {
				case "in":
					r.Op = OpIn
				case "notin":
					r.Op = OpNotIn
				default:
					return fail("expected =, ==, !=, in or notin")
				}
				skip()
				if i == len(in) || in[i] != '(' {
					return fail("expected '('")
				}
				i++
				for {
					skip()
					r.Values = append(r.Values, word())
					skip()
					if i < len(in) && in[i] == ',' {
						i++
						continue
					}
					break
				}
				if i == len(in) || in[i] != ')' {
					return fail("expected ')'")
				}
				i++
			}
		}
		if r.Key == "" {
			return fail("expected a label key")
		}
		if err := checkRequirement(r); err != nil {
			return Selector{}, fmt.Errorf("label selector %q: %v", in, err)
		}
		s.Reqs = append(s.Reqs, r)
		skip()
		if i == len(in) {
			return s, nil
		}
		if in[i] != ',' {
			return fail("expected ','")
		}
		i++
	}
}

// ---- field selectors ----

type FieldRequirement struct {
	Field string
	Value string
	Not   bool
}

// ParseFieldSelector parses "a=b,c!=d". Every field must be in allowed.
func ParseFieldSelector(in string, allowed []string) ([]FieldRequirement, error) {
	var out []FieldRequirement
	if strings.TrimSpace(in) == "" {
		return nil, nil
	}
	for part := range strings.SplitSeq(in, ",") {
		var r FieldRequirement
		var ok bool
		if r.Field, r.Value, ok = strings.Cut(part, "!="); ok {
			r.Not = true
		} else if r.Field, r.Value, ok = strings.Cut(part, "=="); !ok {
			if r.Field, r.Value, ok = strings.Cut(part, "="); !ok {
				return nil, fmt.Errorf("field selector %q: expected field=value or field!=value", part)
			}
		}
		r.Field, r.Value = strings.TrimSpace(r.Field), strings.TrimSpace(r.Value)
		if !slices.Contains(allowed, r.Field) {
			return nil, fmt.Errorf("field selector %q is not supported here; supported fields: %s", r.Field, strings.Join(allowed, ", "))
		}
		out = append(out, r)
	}
	return out, nil
}

func MatchFields(reqs []FieldRequirement, fields map[string]string) bool {
	for _, r := range reqs {
		if (fields[r.Field] == r.Value) == r.Not {
			return false
		}
	}
	return true
}

// Selectable pulls the labels and the selectable fields out of a stored object
// without decoding the whole thing.
func Selectable(raw []byte) (labels, fields map[string]string, err error) {
	var v struct {
		Metadata struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace"`
			Labels    map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
		InvolvedObject ObjectReference `json:"involvedObject"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, nil, err
	}
	io := v.InvolvedObject
	return v.Metadata.Labels, map[string]string{
		"metadata.name":                  v.Metadata.Name,
		"metadata.namespace":             v.Metadata.Namespace,
		"spec.nodeName":                  v.Spec.NodeName,
		"status.phase":                   v.Status.Phase,
		"involvedObject.kind":            io.Kind,
		"involvedObject.namespace":       io.Namespace,
		"involvedObject.name":            io.Name,
		"involvedObject.uid":             io.UID,
		"involvedObject.apiVersion":      io.APIVersion,
		"involvedObject.resourceVersion": io.ResourceVersion,
		"involvedObject.fieldPath":       io.FieldPath,
	}, nil
}
