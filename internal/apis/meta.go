// Package apis holds Shearwater's own API types, defaults and validation.
// Nothing here imports k8s.io modules (design decision 1).
package apis

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"
)

type TypeMeta struct {
	Kind       string `json:"kind,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
}

func (t *TypeMeta) Type() *TypeMeta { return t }

// The patchStrategy and patchMergeKey tags are the strategic merge patch schema
// (internal/patch). They are copied from the Kubernetes types; see the v1.36.6
// generated.proto comments (+patchStrategy, +patchMergeKey).
type ObjectMeta struct {
	Name                       string            `json:"name,omitempty"`
	GenerateName               string            `json:"generateName,omitempty"`
	Namespace                  string            `json:"namespace,omitempty"`
	UID                        string            `json:"uid,omitempty"`
	ResourceVersion            string            `json:"resourceVersion,omitempty"`
	Generation                 int64             `json:"generation,omitempty"`
	CreationTimestamp          *Time             `json:"creationTimestamp,omitempty"`
	DeletionTimestamp          *Time             `json:"deletionTimestamp,omitempty"`
	DeletionGracePeriodSeconds *int64            `json:"deletionGracePeriodSeconds,omitempty"`
	Labels                     map[string]string `json:"labels,omitempty"`
	Annotations                map[string]string `json:"annotations,omitempty"`
	OwnerReferences            []OwnerReference  `json:"ownerReferences,omitempty" patchStrategy:"merge" patchMergeKey:"uid"`
	Finalizers                 []string          `json:"finalizers,omitempty" patchStrategy:"merge"`
}

type OwnerReference struct {
	APIVersion         string `json:"apiVersion"`
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	UID                string `json:"uid"`
	Controller         *bool  `json:"controller,omitempty"`
	BlockOwnerDeletion *bool  `json:"blockOwnerDeletion,omitempty"`
}

// Object is what every stored API type implements.
type Object interface {
	Type() *TypeMeta
	Meta() *ObjectMeta
}

// CopyStatus copies the Status field from src to dst when the type has one.
func CopyStatus(dst, src Object) {
	d := reflect.ValueOf(dst).Elem().FieldByName("Status")
	s := reflect.ValueOf(src).Elem().FieldByName("Status")
	if d.IsValid() && s.IsValid() {
		d.Set(s)
	}
}

// SpecJSON returns the encoded Spec field, or nil when the type has none.
// Two objects have the same spec exactly when these bytes are equal.
func SpecJSON(o Object) []byte {
	f := reflect.ValueOf(o).Elem().FieldByName("Spec")
	if !f.IsValid() {
		return nil
	}
	b, _ := json.Marshal(f.Interface())
	return b
}

// Time is an RFC 3339 timestamp with second precision, always UTC.
type Time struct{ time.Time }

func Now() *Time { return &Time{time.Now().UTC().Truncate(time.Second)} }

func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return []byte(`"` + t.UTC().Format(time.RFC3339) + `"`), nil
}

func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("time must be an RFC 3339 string")
	}
	p, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("time %q is not RFC 3339", s)
	}
	t.Time = p.UTC()
	return nil
}

// IntOrString is a value that is either a number (3) or a string ("25%", "http").
type IntOrString struct {
	IsString bool
	Int      int32
	Str      string
}

func Int(v int32) IntOrString      { return IntOrString{Int: v} }
func Str(v string) IntOrString     { return IntOrString{IsString: true, Str: v} }
func (v IntOrString) IsZero() bool { return !v.IsString && v.Int == 0 }

func (v IntOrString) String() string {
	if v.IsString {
		return v.Str
	}
	return strconv.Itoa(int(v.Int))
}

func (v IntOrString) MarshalJSON() ([]byte, error) {
	if v.IsString {
		return json.Marshal(v.Str)
	}
	return []byte(strconv.Itoa(int(v.Int))), nil
}

func (v *IntOrString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		v.IsString, v.Int = true, 0
		return json.Unmarshal(b, &v.Str)
	}
	n, err := strconv.ParseInt(string(b), 10, 32)
	if err != nil {
		return fmt.Errorf("expected an integer or a string, got %s", b)
	}
	v.IsString, v.Str, v.Int = false, "", int32(n)
	return nil
}

type LabelSelector struct {
	MatchLabels      map[string]string          `json:"matchLabels,omitempty"`
	MatchExpressions []LabelSelectorRequirement `json:"matchExpressions,omitempty"`
}

type LabelSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}

type ObjectReference struct {
	Kind            string `json:"kind,omitempty"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name,omitempty"`
	UID             string `json:"uid,omitempty"`
	APIVersion      string `json:"apiVersion,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	FieldPath       string `json:"fieldPath,omitempty"`
}

// Status is the body of every error response and of a completed delete.
type Status struct {
	TypeMeta
	Metadata struct{}       `json:"metadata"`
	Status   string         `json:"status"`
	Message  string         `json:"message,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Details  *StatusDetails `json:"details,omitempty"`
	Code     int            `json:"code"`
}

type StatusDetails struct {
	Name   string        `json:"name,omitempty"`
	Group  string        `json:"group,omitempty"`
	Kind   string        `json:"kind,omitempty"`
	UID    string        `json:"uid,omitempty"`
	Causes []StatusCause `json:"causes,omitempty"`
}

type StatusCause struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Field   string `json:"field,omitempty"`
}
