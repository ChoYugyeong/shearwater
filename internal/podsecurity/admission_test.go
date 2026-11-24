package podsecurity

import (
	"encoding/json"
	"testing"

	"shearwater/internal/apis"
)

func TestLevels(t *testing.T) {
	restricted := apis.PodSpec{
		SecurityContext: json.RawMessage(`{"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}`),
		Containers: []apis.Container{{
			Name: "worker", SecurityContext: json.RawMessage(`{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}`),
		}},
	}
	if err := check("restricted", &restricted); err != nil {
		t.Fatal(err)
	}
	hostPath := apis.PodSpec{Volumes: []apis.Volume{{Name: "host", HostPath: &apis.HostPathVolumeSource{Path: "/data"}}}}
	for _, test := range []struct {
		level string
		spec  apis.PodSpec
		ok    bool
	}{
		{"", hostPath, true},
		{"privileged", hostPath, true},
		{"baseline", hostPath, false},
		{"restricted", hostPath, false},
		{"baseline", apis.PodSpec{Containers: []apis.Container{{Name: "web"}}}, true},
		{"restricted", apis.PodSpec{Containers: []apis.Container{{Name: "web"}}}, false},
		{"baseline", apis.PodSpec{Containers: []apis.Container{{SecurityContext: json.RawMessage(`{"privileged":true}`)}}}, false},
		{"baseline", apis.PodSpec{Containers: []apis.Container{{SecurityContext: json.RawMessage(`{"capabilities":{"add":["SYS_ADMIN"]}}`)}}}, false},
	} {
		if err := check(test.level, &test.spec); (err == nil) != test.ok {
			t.Errorf("level %q spec %+v: %v", test.level, test.spec, err)
		}
	}
}
