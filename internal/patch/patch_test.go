package patch_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"shearwater/internal/apis"
	"shearwater/internal/patch"
)

func parse(t *testing.T, s string) any {
	t.Helper()
	v, err := patch.Parse([]byte(s))
	if err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return v
}

func show(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestStrategic runs every case of testdata/smp.json. The expected results were
// produced by Kubernetes itself (apimachinery v0.36.6; see the file's "source").
func TestStrategic(t *testing.T) {
	raw, err := os.ReadFile("testdata/smp.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Bases map[string]json.RawMessage
		Cases []struct {
			Name, Kind, Base, Error string
			Original, Patch         json.RawMessage
			Expected                json.RawMessage
			OursError               bool
		}
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	types := map[string]reflect.Type{
		"Pod": reflect.TypeFor[apis.Pod](), "Deployment": reflect.TypeFor[apis.Deployment](), "Service": reflect.TypeFor[apis.Service](),
	}
	if len(f.Cases) < 70 {
		t.Fatalf("only %d cases loaded", len(f.Cases))
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			orig := c.Original
			if c.Base != "" {
				orig = f.Bases[c.Base]
			}
			doc := parse(t, string(orig)).(map[string]any)
			before := show(doc)
			got, err := patch.Strategic(doc, parse(t, string(c.Patch)).(map[string]any), types[c.Kind])
			if show(doc) != before {
				t.Fatalf("the original was modified")
			}
			if c.Error != "" || c.OursError {
				if err == nil {
					t.Fatalf("no error; Kubernetes says %q; got %s", c.Error, show(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if want := parse(t, string(c.Expected)); !patch.Equal(got, want) {
				t.Fatalf("\n got %s\nwant %s", show(got), show(want))
			}
		})
	}
}

// RFC 7386, appendix A.
func TestMerge(t *testing.T) {
	for _, c := range [][3]string{
		{`{"a":"b"}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"b"}`, `{"b":"c"}`, `{"a":"b","b":"c"}`},
		{`{"a":"b"}`, `{"a":null}`, `{}`},
		{`{"a":"b","b":"c"}`, `{"a":null}`, `{"b":"c"}`},
		{`{"a":["b"]}`, `{"a":"c"}`, `{"a":"c"}`},
		{`{"a":"c"}`, `{"a":["b"]}`, `{"a":["b"]}`},
		{`{"a":{"b":"c"}}`, `{"a":{"b":"d","c":null}}`, `{"a":{"b":"d"}}`},
		{`{"a":[{"b":"c"}]}`, `{"a":[1]}`, `{"a":[1]}`},
		{`["a","b"]`, `["c","d"]`, `["c","d"]`},
		{`{"a":"b"}`, `["c"]`, `["c"]`},
		{`{"a":"foo"}`, `null`, `null`},
		{`{"a":"foo"}`, `"bar"`, `"bar"`},
		{`{"e":null}`, `{"a":1}`, `{"e":null,"a":1}`},
		{`[1,2]`, `{"a":"b","c":null}`, `{"a":"b"}`},
		{`{}`, `{"a":{"bb":{"ccc":null}}}`, `{"a":{"bb":{}}}`},
	} {
		doc := parse(t, c[0])
		if got := patch.Merge(doc, parse(t, c[1])); !patch.Equal(got, parse(t, c[2])) {
			t.Errorf("Merge(%s, %s) = %s, want %s", c[0], c[1], show(got), c[2])
		}
		if show(doc) != show(parse(t, c[0])) {
			t.Errorf("Merge modified its input %s", c[0])
		}
	}
}

// RFC 6902, appendix A, and the error cases that matter for an API object.
func TestJSONPatch(t *testing.T) {
	for _, c := range []struct{ doc, ops, want, err string }{
		{`{"foo":"bar"}`, `[{"op":"add","path":"/baz","value":"qux"}]`, `{"baz":"qux","foo":"bar"}`, ""},
		{`{"foo":["bar","baz"]}`, `[{"op":"add","path":"/foo/1","value":"qux"}]`, `{"foo":["bar","qux","baz"]}`, ""},
		{`{"baz":"qux","foo":"bar"}`, `[{"op":"remove","path":"/baz"}]`, `{"foo":"bar"}`, ""},
		{`{"foo":["bar","qux","baz"]}`, `[{"op":"remove","path":"/foo/1"}]`, `{"foo":["bar","baz"]}`, ""},
		{`{"baz":"qux","foo":"bar"}`, `[{"op":"replace","path":"/baz","value":"boo"}]`, `{"baz":"boo","foo":"bar"}`, ""},
		{`{"foo":{"bar":"baz","waldo":"fred"},"qux":{"corge":"grault"}}`, `[{"op":"move","from":"/foo/waldo","path":"/qux/thud"}]`,
			`{"foo":{"bar":"baz"},"qux":{"corge":"grault","thud":"fred"}}`, ""},
		{`{"foo":["all","grass","cows","eat"]}`, `[{"op":"move","from":"/foo/1","path":"/foo/3"}]`, `{"foo":["all","cows","eat","grass"]}`, ""},
		{`{"baz":"qux","foo":["a",2,"c"]}`, `[{"op":"test","path":"/baz","value":"qux"},{"op":"test","path":"/foo/1","value":2}]`, `{"baz":"qux","foo":["a",2,"c"]}`, ""},
		{`{"baz":"qux"}`, `[{"op":"test","path":"/baz","value":"bar"}]`, "", "is \"qux\", not \"bar\""},
		{`{"foo":"bar"}`, `[{"op":"add","path":"/child","value":{"grandchild":{}}}]`, `{"foo":"bar","child":{"grandchild":{}}}`, ""},
		{`{"foo":"bar"}`, `[{"op":"add","path":"/baz","value":"qux","xyz":123}]`, `{"foo":"bar","baz":"qux"}`, ""},
		{`{"foo":"bar"}`, `[{"op":"add","path":"/baz/bat","value":"qux"}]`, "", "no member \"baz\""},
		{`{"/":9,"~1":10}`, `[{"op":"test","path":"/~01","value":10}]`, `{"/":9,"~1":10}`, ""},
		{`{"/":9,"~1":10}`, `[{"op":"test","path":"/~01","value":"10"}]`, "", "not"},
		{`{"foo":["bar"]}`, `[{"op":"add","path":"/foo/-","value":["abc","def"]}]`, `{"foo":["bar",["abc","def"]]}`, ""},
		{`{"a":{"b":1}}`, `[{"op":"copy","from":"/a","path":"/c"},{"op":"replace","path":"/c/b","value":2}]`, `{"a":{"b":1},"c":{"b":2}}`, ""},
		{`{"a":[1]}`, `[{"op":"add","path":"/a/2","value":3}]`, "", "out of range"},
		{`{"a":[1]}`, `[{"op":"remove","path":"/a/01"}]`, "", "not an array index"},
		{`{"a":1}`, `[{"op":"remove","path":"/b"}]`, "", "no member"},
		{`{"a":1}`, `[{"op":"replace","path":"/b","value":1}]`, "", "no member"},
		{`{"a":1}`, `[{"op":"frobnicate","path":"/a"}]`, "", "unknown operation"},
		{`{"a":1}`, `[{"op":"add","value":1}]`, "", "\"path\" is missing"},
		{`{"a":1}`, `[{"op":"add","path":"a","value":1}]`, "", "must start with"},
		{`{"a":{"b":1}}`, `[{"op":"move","from":"/a","path":"/a/b/c"}]`, "", "into itself"},
		{`{"a":1}`, `{"op":"add"}`, "", "array of operations"},
		{`{"a":1}`, `[{"op":"replace","path":"","value":{"z":1}}]`, `{"z":1}`, ""},
		{`{"spec":{"containers":[{"name":"a","image":"x"}]}}`, `[{"op":"replace","path":"/spec/containers/0/image","value":"y"}]`, `{"spec":{"containers":[{"name":"a","image":"y"}]}}`, ""},
	} {
		doc := parse(t, c.doc)
		got, err := patch.JSONPatch(doc, []byte(c.ops))
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s on %s: error %v, want one containing %q", c.ops, c.doc, err, c.err)
			}
		case err != nil:
			t.Errorf("%s on %s: %v", c.ops, c.doc, err)
		case !patch.Equal(got, parse(t, c.want)):
			t.Errorf("%s on %s = %s, want %s", c.ops, c.doc, show(got), c.want)
		}
		if show(doc) != show(parse(t, c.doc)) {
			t.Errorf("JSONPatch modified its input %s", c.doc)
		}
	}
}
