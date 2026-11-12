package protobuf_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"shearwater/internal/apis"
	"shearwater/internal/protobuf"
)

const (
	linuxDir   = "testdata/kubectl-v1.36.6"
	windowsDir = "testdata/kubectl-v1.36.0-windows"
)

func newObject(t *testing.T, kind string) apis.Object {
	t.Helper()
	for _, r := range apis.Resources {
		if r.Kind == kind {
			return r.New()
		}
	}
	t.Fatalf("no resource of kind %s", kind)
	return nil
}

// decodeLikeTheServer does what the API server does with a JSON body.
func decodeLikeTheServer(t *testing.T, kind string, js []byte) (string, []string) {
	t.Helper()
	obj := newObject(t, kind)
	unknown, err := apis.Decode(js, obj, "status")
	if err != nil {
		t.Fatalf("%v\n%s", err, js)
	}
	out, _ := json.Marshal(obj)
	return string(out), unknown
}

// Every body kubectl really sent must decode to the object Kubernetes itself
// decodes from it (NAME.json; see testdata/README.md for where it came from).
func TestKubectlBodies(t *testing.T) {
	bins, _ := filepath.Glob(linuxDir + "/*.bin")
	if len(bins) < 10 {
		t.Fatalf("found %d bodies, expected at least 10", len(bins))
	}
	for _, bin := range bins {
		name := strings.TrimSuffix(filepath.Base(bin), ".bin")
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			win, err := os.ReadFile(filepath.Join(windowsDir, name+".bin"))
			if err != nil || !bytes.Equal(win, body) {
				t.Errorf("the kubectl.exe v1.36.0 body differs from the v1.36.6 one (%v)", err)
			}
			want, err := os.ReadFile(filepath.Join(linuxDir, name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var head apis.TypeMeta
			json.Unmarshal(want, &head)

			apiVersion, kind, js, err := protobuf.Decode(body)
			if err != nil {
				t.Fatal(err)
			}
			if apiVersion != head.APIVersion || kind != head.Kind {
				t.Fatalf("type %s %s, want %s %s", apiVersion, kind, head.APIVersion, head.Kind)
			}
			gotObj, gotUnknown := decodeLikeTheServer(t, kind, js)
			wantObj, wantUnknown := decodeLikeTheServer(t, kind, want)
			if gotObj != wantObj {
				t.Errorf("\n got %s\nwant %s\njson %s", gotObj, wantObj, js)
			}
			if !slices.Equal(gotUnknown, wantUnknown) {
				t.Errorf("unknown fields %v, want %v", gotUnknown, wantUnknown)
			}
		})
	}
}

func decodeFile(t *testing.T, name string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(linuxDir, name+".bin"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, js, err := protobuf.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	json.Unmarshal(js, &v)
	return v
}

func at(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			l, _ := v.([]any)
			if k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

// What must survive although it looks empty, and what must not appear although it is on the wire.
func TestPresence(t *testing.T) {
	zero := decodeFile(t, "create-deployment-zero-replicas")
	if r, ok := at(zero, "spec").(map[string]any)["replicas"]; !ok || r != float64(0) {
		t.Errorf("replicas: 0 was lost: %v", at(zero, "spec"))
	}
	spec := at(zero, "spec").(map[string]any)
	for _, k := range []string{"paused", "minReadySeconds", "strategy", "revisionHistoryLimit"} {
		if _, ok := spec[k]; ok {
			t.Errorf("spec.%s is in the JSON although kubectl only wrote its zero value", k)
		}
	}
	if _, ok := zero["status"]; ok {
		t.Errorf("status was taken from the request")
	}
	if got := at(zero, "spec", "template", "spec", "containers", 0, "ports", 0, "containerPort"); got != float64(8080) {
		t.Errorf("containerPort = %v", got)
	}

	pod := decodeFile(t, "debug-copy-pod")
	if e, ok := at(pod, "spec", "volumes", 0).(map[string]any)["emptyDir"]; !ok || len(e.(map[string]any)) != 0 {
		t.Errorf("emptyDir: {} was lost: %v", at(pod, "spec", "volumes"))
	}
	if got := at(pod, "spec", "containers", 0, "resources", "limits", "cpu"); got != "500m" {
		t.Errorf("limits.cpu = %v", got)
	}
	env := at(pod, "spec", "containers", 0, "env").([]any)
	if len(env) != 2 || at(env[1], "name") != "EMPTY" {
		t.Errorf("env = %v", env)
	}
	if _, ok := env[1].(map[string]any)["value"]; ok {
		t.Errorf("an env value that was not set appeared: %v", env[1])
	}

	two := decodeFile(t, "create-service-clusterip-two-ports")
	if got := at(two, "spec", "ports", 1, "targetPort"); got != "https" {
		t.Errorf("a named target port came out as %v", got)
	}
	if got := at(two, "spec", "ports", 0, "targetPort"); got != float64(8080) {
		t.Errorf("a numeric target port came out as %v", got)
	}
	if got := at(decodeFile(t, "create-service-clusterip-headless"), "spec", "clusterIP"); got != "None" {
		t.Errorf("clusterIP = %v", got)
	}
}

// ---- hand-built messages for what kubectl does not happen to send ----

func varint(v uint64) []byte {
	var b []byte
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func vfield(num int, v uint64) []byte { return append(varint(uint64(num)<<3), varint(v)...) }
func bfield(num int, p []byte) []byte {
	return append(append(varint(uint64(num)<<3|2), varint(uint64(len(p)))...), p...)
}
func sfield(num int, s string) []byte { return bfield(num, []byte(s)) }
func cat(parts ...[]byte) []byte      { return bytes.Join(parts, nil) }

func envelope(apiVersion, kind string, raw []byte) []byte {
	return cat([]byte("k8s\x00"), bfield(1, cat(sfield(1, apiVersion), sfield(2, kind))), bfield(2, raw))
}

func podBody(spec []byte) []byte {
	return envelope("v1", "Pod", cat(bfield(1, sfield(1, "p")), bfield(2, spec)))
}

func mustDecode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	_, _, js, err := protobuf.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	json.Unmarshal(js, &v)
	return v
}

func TestHandBuilt(t *testing.T) {
	c := bfield(2, cat(sfield(1, "c"), sfield(2, "img")))

	// A pointer bool set to false is kept; a plain bool set to false is not.
	v := mustDecode(t, podBody(cat(c, vfield(30, 0), vfield(11, 0))))
	spec := at(v, "spec").(map[string]any)
	if got, ok := spec["enableServiceLinks"]; !ok || got != false {
		t.Errorf("enableServiceLinks: false was lost: %v", spec)
	}
	if _, ok := spec["hostNetwork"]; ok {
		t.Errorf("hostNetwork: false should read as not set: %v", spec)
	}

	// A scalar that appears twice: the last one wins, also when it is the zero value.
	v = mustDecode(t, podBody(cat(c, sfield(10, "n1"), sfield(10, "n2"))))
	if got := at(v, "spec", "nodeName"); got != "n2" {
		t.Errorf("nodeName = %v, want the last value", got)
	}
	v = mustDecode(t, podBody(cat(c, sfield(10, "n1"), sfield(10, ""))))
	if got := at(v, "spec", "nodeName"); got != nil {
		t.Errorf("nodeName = %v, want it unset by the later empty value", got)
	}

	// A message that appears twice is merged; repeated fields are appended; order does not matter.
	body := envelope("v1", "Pod", cat(bfield(2, c), bfield(1, sfield(1, "p")), bfield(2, cat(sfield(10, "n1"), c)), bfield(1, sfield(3, "ns"))))
	v = mustDecode(t, body)
	if l, _ := at(v, "spec", "containers").([]any); len(l) != 2 || at(v, "spec", "nodeName") != "n1" {
		t.Errorf("spec was not merged: %v", at(v, "spec"))
	}
	if at(v, "metadata", "name") != "p" || at(v, "metadata", "namespace") != "ns" {
		t.Errorf("metadata was not merged: %v", at(v, "metadata"))
	}

	// Negative numbers are ten-byte varints.
	v = mustDecode(t, podBody(cat(c, vfield(4, ^uint64(0)))))
	if got := at(v, "spec", "terminationGracePeriodSeconds"); got != float64(-1) {
		t.Errorf("terminationGracePeriodSeconds = %v, want -1", got)
	}

	// Labels may have empty values; an unset creation time is not 1970.
	meta := cat(sfield(1, "p"), bfield(8, nil), bfield(11, cat(sfield(1, "a"), sfield(2, ""))), bfield(11, cat(sfield(1, "b"), sfield(2, "x"))))
	v = mustDecode(t, envelope("v1", "Pod", cat(bfield(1, meta), bfield(2, c))))
	if l := at(v, "metadata", "labels").(map[string]any); l["a"] != "" || l["b"] != "x" || len(l) != 2 {
		t.Errorf("labels = %v", l)
	}
	if got, ok := at(v, "metadata").(map[string]any)["creationTimestamp"]; ok {
		t.Errorf("creationTimestamp = %v, want it absent", got)
	}
	v = mustDecode(t, envelope("v1", "Pod", cat(bfield(1, cat(sfield(1, "p"), bfield(8, vfield(1, 1700000000)))), bfield(2, c))))
	if got := at(v, "metadata", "creationTimestamp"); got != "2023-11-14T22:13:20Z" {
		t.Errorf("creationTimestamp = %v", got)
	}

	// A field Kubernetes has and Shearwater does not is passed on under its name,
	// for the unknown-field rule to judge.
	v = mustDecode(t, podBody(cat(c, vfield(12, 1))))
	if got := at(v, "spec", "hostPID"); got != true {
		t.Errorf("hostPID = %v", got)
	}

	// maxSurge: 0 is a pointer in the API type and must be kept.
	ru := cat(bfield(1, cat(vfield(1, 0), vfield(2, 0))), bfield(2, cat(vfield(1, 1), sfield(3, "50%"))))
	dep := envelope("apps/v1", "Deployment", cat(bfield(1, sfield(1, "d")), bfield(2, bfield(4, cat(sfield(1, "RollingUpdate"), bfield(2, ru))))))
	v = mustDecode(t, dep)
	if got := at(v, "spec", "strategy", "rollingUpdate"); !equalJSON(got, `{"maxUnavailable":0,"maxSurge":"50%"}`) {
		t.Errorf("rollingUpdate = %v", got)
	}
}

func equalJSON(v any, want string) bool {
	a, _ := json.Marshal(v)
	var w any
	json.Unmarshal([]byte(want), &w)
	b, _ := json.Marshal(w)
	return string(a) == string(b)
}

func TestRefusals(t *testing.T) {
	c := bfield(2, cat(sfield(1, "c"), sfield(2, "img")))
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"no marker", []byte(`{"kind":"Pod"}`), "marker"},
		{"a kind without tables", envelope("v1", "ConfigMap", nil), "not v1 ConfigMap"},
		{"a field number that is not in the tables", podBody(cat(c, vfield(99, 1))), "PodSpec: field number 99"},
		{"a message Shearwater cannot read (affinity)", podBody(cat(c, bfield(18, sfield(1, "x")))), "PodSpec.affinity"},
		{"...also when it is empty, because it is a pointer", podBody(cat(c, bfield(18, nil))), "PodSpec.affinity"},
		{"a probe", podBody(bfield(2, cat(sfield(1, "c"), bfield(10, vfield(2, 5))))), "Container.livenessProbe"},
		{"a volume source without a table", podBody(cat(c, bfield(1, cat(sfield(1, "v"), bfield(2, bfield(6, sfield(1, "s"))))))), "VolumeSource.secret"},
		{"wrong wire type", podBody(cat(c, sfield(11, "yes"))), "PodSpec.hostNetwork: wire type"},
		{"a length that runs past the end", podBody(append(varint(2<<3|2), 0x7f)), "past the end"},
		{"a number that never ends", podBody(bytes.Repeat([]byte{0xff}, 12)), "64 bits"},
		{"content encoding", cat(envelope("v1", "Pod", nil), sfield(3, "gzip")), "content encoding"},
		{"invalid UTF-8 in a string", podBody(cat(c, bfield(10, []byte{0xff, 0xfe}))), "UTF-8"},
	} {
		_, _, _, err := protobuf.Decode(tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one containing %q", tc.name, err, tc.want)
		}
	}

	// Damaged input: every truncation and every single-byte change of a real
	// body must end in a result or an error, never a panic.
	for _, bin := range must(filepath.Glob(linuxDir + "/*.bin")) {
		body, _ := os.ReadFile(bin)
		for i := 0; i < len(body); i++ {
			protobuf.Decode(body[:i])
			damaged := bytes.Clone(body)
			damaged[i] ^= 0xff
			protobuf.Decode(damaged)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
