package apis

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseSelector(t *testing.T) {
	web := map[string]string{"app": "web", "tier": "front"}
	cases := []struct {
		in    string
		match bool
	}{
		{"", true},
		{"app=web", true},
		{"app==web", true},
		{"app = web , tier=front", true},
		{"app!=web", false},
		{"app!=db", true},
		{"missing!=x", true}, // != also matches objects without the key
		{"app in (web, db)", true},
		{"app in (db)", false},
		{"app notin (db)", true},
		{"missing notin (db)", true},
		{"app", true},
		{"missing", false},
		{"!missing", true},
		{"!app", false},
		{"app=web,!missing,tier in (front,back)", true},
		{"example.com/role=x", false},
		{"app=", false}, // empty value is a real value
	}
	for _, c := range cases {
		s, err := ParseSelector(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got := s.Matches(web); got != c.match {
			t.Errorf("%q: match = %v, want %v", c.in, got, c.match)
		}
	}
	for _, bad := range []string{"app=web,", "=web", "app in web", "app in (web", "app > 3", "a b", "-bad=x", "app=web tier=front"} {
		if _, err := ParseSelector(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestSelectorFrom(t *testing.T) {
	if s, _ := SelectorFrom(nil); s.Matches(map[string]string{"a": "b"}) {
		t.Error("a nil selector must match nothing")
	}
	if s, _ := SelectorFrom(&LabelSelector{}); !s.Matches(nil) {
		t.Error("an empty selector must match everything")
	}
	s, err := SelectorFrom(&LabelSelector{
		MatchLabels:      map[string]string{"app": "web"},
		MatchExpressions: []LabelSelectorRequirement{{Key: "tier", Operator: "NotIn", Values: []string{"db"}}, {Key: "canary", Operator: "DoesNotExist"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Matches(map[string]string{"app": "web", "tier": "front"}) || s.Matches(map[string]string{"app": "web", "canary": "1"}) {
		t.Error("matchLabels + matchExpressions gave the wrong answer")
	}
	if _, err := SelectorFrom(&LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "a", Operator: "In"}}}); err == nil {
		t.Error("In without values must be an error")
	}
}

func TestFieldSelector(t *testing.T) {
	allowed := []string{"metadata.name", "status.phase"}
	reqs, err := ParseFieldSelector("metadata.name=web,status.phase!=Failed", allowed)
	if err != nil {
		t.Fatal(err)
	}
	if !MatchFields(reqs, map[string]string{"metadata.name": "web", "status.phase": "Running"}) ||
		MatchFields(reqs, map[string]string{"metadata.name": "web", "status.phase": "Failed"}) {
		t.Error("field selector gave the wrong answer")
	}
	if _, err := ParseFieldSelector("spec.foo=1", allowed); err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("an unsupported field must be refused and list the supported ones, got %v", err)
	}
}

func TestQuantity(t *testing.T) {
	cases := []struct {
		in           string
		value, milli int64
	}{
		{"1", 1, 1000}, {"500m", 1, 500}, {"0.5", 1, 500}, {"1.5", 2, 1500},
		{"256Mi", 256 << 20, 256 << 20 * 1000}, {"1Gi", 1 << 30, 1 << 30 * 1000},
		{"2k", 2000, 2000000}, {"1e3", 1000, 1000000}, {"100", 100, 100000}, {"0", 0, 0},
	}
	for _, c := range cases {
		q := Quantity(c.in)
		v, err := q.Value()
		if err != nil || v != c.value {
			t.Errorf("%s: Value = %d, %v; want %d", c.in, v, err, c.value)
		}
		m, err := q.MilliValue()
		if err != nil || m != c.milli {
			t.Errorf("%s: MilliValue = %d, %v; want %d", c.in, m, err, c.milli)
		}
	}
	for _, bad := range []string{"", "abc", "1.2.3", "1Kb", "m", "1 Gi", "1e", "--1"} {
		if _, err := ParseQuantity(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	var r ResourceRequirements
	if err := json.Unmarshal([]byte(`{"limits":{"cpu":1,"memory":"64Mi"}}`), &r); err != nil || r.Limits["cpu"] != "1" {
		t.Errorf("a bare number must be accepted as a quantity: %v %v", r, err)
	}
}

func TestDecodeReportsUnknownFields(t *testing.T) {
	body := `{"apiVersion":"v1","kind":"Pod",
	  "metadata":{"name":"p","creationTimestamp":null,"labels":{"a":"b"},"bogus":1},
	  "spec":{"containers":[{"name":"c","image":"i","resources":{},"oops":{"x":1},"livenessProbe":{"anything":{"goes":null}}}],
	          "dnsPolicy":"ClusterFirst","affinity":null,"Containers":[]},
	  "status":{"whatever":true}, "extra":[]}`
	var p Pod
	unknown, err := Decode([]byte(body), &p, "status")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"extra", "metadata.bogus", "spec.Containers", "spec.containers[0].oops"}
	if !reflect.DeepEqual(unknown, want) {
		t.Errorf("unknown = %v, want %v", unknown, want)
	}
	if p.Metadata.CreationTimestamp != nil || p.Spec.Affinity != nil || p.Status.Phase != "" {
		t.Errorf("null members and dropped members must not reach the object: %+v", p)
	}
	if len(p.Spec.Containers) != 1 || p.Spec.DNSPolicy != "ClusterFirst" || len(p.Spec.Containers[0].LivenessProbe) == 0 {
		t.Errorf("known members were lost: %+v", p.Spec)
	}

	if _, err := Decode([]byte(`{"spec":{"containers":"nope"}}`), &Pod{}); err == nil {
		t.Error("a wrong type must be an error")
	}
	if _, err := Decode([]byte(`{} {}`), &Pod{}); err == nil {
		t.Error("trailing data must be an error")
	}
	if _, err := Decode([]byte(`[]`), &Pod{}); err == nil {
		t.Error("a non-object body must be an error")
	}
}

func reasons(l ErrorList) string {
	var parts []string
	for _, e := range l {
		parts = append(parts, e.String())
	}
	return strings.Join(parts, "\n")
}

func decodeInto[T any](t *testing.T, body string) *T {
	t.Helper()
	v := new(T)
	if unknown, err := Decode([]byte(body), v); err != nil || len(unknown) > 0 {
		t.Fatalf("decode: %v unknown=%v", err, unknown)
	}
	return v
}

func TestDeploymentDefaultsAndValidation(t *testing.T) {
	d := decodeInto[Deployment](t, `{"metadata":{"name":"web"},"spec":{"selector":{"matchLabels":{"app":"web"}},
	  "template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"nginx","image":"nginx:1.27","ports":[{"containerPort":80}]}]}}}}`)
	DefaultDeployment(d)
	if errs := ValidateDeployment(d); len(errs) > 0 {
		t.Fatalf("a minimal deployment must be valid:\n%s", reasons(errs))
	}
	s := d.Spec
	c := s.Template.Spec.Containers[0]
	if *s.Replicas != 1 || s.Strategy.Type != "RollingUpdate" || s.Strategy.RollingUpdate.MaxSurge.Str != "25%" ||
		s.Strategy.RollingUpdate.MaxUnavailable.Str != "25%" || *s.RevisionHistoryLimit != 10 ||
		s.Template.Spec.RestartPolicy != "Always" || *s.Template.Spec.TerminationGracePeriodSeconds != 30 ||
		c.ImagePullPolicy != "IfNotPresent" || c.Ports[0].Protocol != "TCP" {
		b, _ := json.Marshal(d.Spec)
		t.Errorf("defaults are wrong: %s", b)
	}

	bad := []struct{ name, body, want string }{
		{"selector mismatch", `{"metadata":{"name":"web"},"spec":{"selector":{"matchLabels":{"app":"x"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"c","image":"i"}]}}}}`, "does not match template"},
		{"no selector", `{"metadata":{"name":"web"},"spec":{"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"c","image":"i"}]}}}}`, "spec.selector: Required"},
		{"no containers", `{"metadata":{"name":"web"},"spec":{"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{}}}}`, "spec.template.spec.containers: Required"},
		{"bad name", `{"metadata":{"name":"Web_1"},"spec":{"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"i"}]}}}}`, "metadata.name: Invalid value"},
		{"both zero", `{"metadata":{"name":"web"},"spec":{"strategy":{"rollingUpdate":{"maxSurge":0,"maxUnavailable":"0%"}},"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"containers":[{"name":"c","image":"i"}]}}}}`, "may not be 0"},
		{"restart policy", `{"metadata":{"name":"web"},"spec":{"selector":{"matchLabels":{"a":"b"}},"template":{"metadata":{"labels":{"a":"b"}},"spec":{"restartPolicy":"Never","containers":[{"name":"c","image":"i"}]}}}}`, "spec.template.spec.restartPolicy"},
	}
	for _, b := range bad {
		d := decodeInto[Deployment](t, b.body)
		DefaultDeployment(d)
		if got := reasons(ValidateDeployment(d)); !strings.Contains(got, b.want) {
			t.Errorf("%s: want an error containing %q, got:\n%s", b.name, b.want, got)
		}
	}
}

func TestPodRejectedFields(t *testing.T) {
	p := decodeInto[Pod](t, `{"metadata":{"name":"p"},"spec":{"hostNetwork":true,"serviceAccountName":"x",
	  "initContainers":[{"name":"i","image":"i"}],
	  "volumes":[{"name":"data","emptyDir":{}}],
	  "containers":[{"name":"c","image":"nginx","envFrom":[{"configMapRef":{"name":"x"}}],
	    "env":[{"name":"A","valueFrom":{"fieldRef":{"fieldPath":"metadata.name"}}}],
	    "resources":{"limits":{"cpu":"500m","nvidia.com/gpu":1}},
	    "volumeMounts":[{"name":"nope","mountPath":"/x"}]}]}}`)
	DefaultPod(p)
	errs := ValidatePod(p)
	got := reasons(errs)
	for _, want := range []string{"spec.hostNetwork", "spec.serviceAccountName", "spec.initContainers", "envFrom", "valueFrom", "nvidia.com/gpu", "volumeMounts[0].name"} {
		if !strings.Contains(got, want) {
			t.Errorf("want an error about %s, got:\n%s", want, got)
		}
	}
	if p.Spec.Containers[0].ImagePullPolicy != "Always" {
		t.Errorf("an image without a tag defaults to imagePullPolicy Always, got %s", p.Spec.Containers[0].ImagePullPolicy)
	}

	old := decodeInto[Pod](t, `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c","image":"nginx:1"}]}}`)
	DefaultPod(old)
	img := decodeInto[Pod](t, `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c","image":"nginx:2"}]}}`)
	DefaultPod(img)
	if errs := ValidatePodUpdate(img, old); len(errs) > 0 {
		t.Errorf("changing the image must be allowed:\n%s", reasons(errs))
	}
	cmd := decodeInto[Pod](t, `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c","image":"nginx:1","command":["sh"]}]}}`)
	DefaultPod(cmd)
	if errs := ValidatePodUpdate(cmd, old); len(errs) == 0 {
		t.Error("changing the command of a pod must be refused")
	}
}

func TestServiceDefaultsAndValidation(t *testing.T) {
	s := decodeInto[Service](t, `{"metadata":{"name":"web"},"spec":{"selector":{"app":"web"},"ports":[{"port":80}]}}`)
	DefaultService(s)
	if errs := ValidateService(s); len(errs) > 0 {
		t.Fatalf("a minimal service must be valid:\n%s", reasons(errs))
	}
	if p := s.Spec.Ports[0]; s.Spec.Type != "ClusterIP" || p.Protocol != "TCP" || p.TargetPort.Int != 80 {
		t.Errorf("defaults are wrong: %+v", s.Spec)
	}
	bad := []struct{ body, want string }{
		{`{"metadata":{"name":"web"},"spec":{"type":"NodePort","ports":[{"port":80}]}}`, "spec.type"},
		{`{"metadata":{"name":"web"},"spec":{"ports":[{"port":53,"protocol":"UDP"}]}}`, "TCP only"},
		{`{"metadata":{"name":"web"},"spec":{"clusterIP":"192.168.0.1","ports":[{"port":80}]}}`, "service range"},
		{`{"metadata":{"name":"web"},"spec":{"ports":[{"port":80},{"port":443}]}}`, "spec.ports[0].name: Required"},
		{`{"metadata":{"name":"1web"},"spec":{"ports":[{"port":80}]}}`, "metadata.name"},
		{`{"metadata":{"name":"web"},"spec":{}}`, "spec.ports: Required"},
	}
	for _, b := range bad {
		s := decodeInto[Service](t, b.body)
		DefaultService(s)
		if got := reasons(ValidateService(s)); !strings.Contains(got, b.want) {
			t.Errorf("want an error containing %q, got:\n%s", b.want, got)
		}
	}
}
