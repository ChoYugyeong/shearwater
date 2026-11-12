package apis

import (
	"slices"
	"testing"
)

func TestDuplicateFields(t *testing.T) {
	for _, c := range []struct {
		name, body string
		drop       []string
		want       []string
	}{
		{"nothing repeated", `{"kind":"Pod","metadata":{"name":"a"},"spec":{"containers":[{"name":"c"},{"name":"d"}]}}`, nil, nil},
		{"a scalar", `{"spec":{"replicas":1,"replicas":2}}`, nil, []string{"spec.replicas"}},
		{"a top-level member", `{"kind":"Pod","kind":"Deployment"}`, nil, []string{"kind"}},
		{"a whole object, and a member inside each copy", `{"spec":{"a":1,"a":2},"spec":{"b":1}}`, nil, []string{"spec", "spec.a"}},
		{"inside a list element", `{"spec":{"containers":[{"name":"c"},{"name":"d","image":"x","name":"e"}]}}`, nil, []string{"spec.containers[1].name"}},
		{"a map key", `{"metadata":{"labels":{"app":"a","tier":"b","app":"c"}}}`, nil, []string{"metadata.labels.app"}},
		{"three times is reported once", `{"a":1,"a":2,"a":3}`, nil, []string{"a"}},
		{"inside a value that is stored as it is", `{"spec":{"affinity":{"nodeAffinity":{"x":1,"x":2}}}}`, nil, []string{"spec.affinity.nodeAffinity.x"}},
		{"nested lists", `{"a":[[{"b":1,"b":2}]]}`, nil, []string{"a[0][0].b"}},
		{"several, sorted", `{"z":1,"z":2,"a":{"b":1,"b":1}}`, nil, []string{"a.b", "z"}},
		{"a dropped member is not looked into", `{"status":{"phase":"a","phase":"b"},"status":{},"spec":{"x":1,"x":2}}`, []string{"status"}, []string{"spec.x"}},
		{"only the top level is dropped by name", `{"spec":{"status":{"a":1,"a":2}}}`, []string{"status"}, []string{"spec.status.a"}},
		{"the same key in sibling objects is not a repeat", `{"a":{"x":1},"b":{"x":1},"c":[{"x":1},{"x":2}]}`, nil, nil},
		{"an array at the top", `[{"op":"add","op":"remove"}]`, nil, []string{"[0].op"}},
		{"not JSON: what was found before the error", `{"a":1,"a":2,`, nil, []string{"a"}},
		{"not an object", `3`, nil, nil},
		{"empty", ``, nil, nil},
	} {
		if got := DuplicateFields([]byte(c.body), c.drop...); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
