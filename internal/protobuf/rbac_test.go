package protobuf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"shearwater/internal/protobuf"
)

// Captured from kubectl v1.36.0 for Windows against a loopback HTTP recorder.
func TestAccessControlBodies(t *testing.T) {
	const rule = `[{"verbs":["get"],"apiGroups":[""],"resources":["pods"]}]`
	const subject = `[{"kind":"User","apiGroup":"rbac.authorization.k8s.io","name":"alice"}]`
	for _, tc := range []struct {
		resource, field, want string
	}{
		{"roles", "rules", rule},
		{"clusterroles", "rules", rule},
		{"rolebindings", "subjects", subject},
		{"clusterrolebindings", "roleRef", `{"apiGroup":"rbac.authorization.k8s.io","kind":"ClusterRole","name":"sample"}`},
		{"resourcequotas", "spec", `{"hard":{"pods":"2"}}`},
		{"selfsubjectaccessreviews", "spec", `{"resourceAttributes":{"namespace":"team-a","verb":"list","resource":"pods"}}`},
		{"selfsubjectrulesreviews", "spec", `{"namespace":"team-a"}`},
		{"selfsubjectreviews", "kind", `"SelfSubjectReview"`},
	} {
		t.Run(tc.resource, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", "access", tc.resource+".bin"))
			if err != nil {
				t.Fatal(err)
			}
			_, _, js, err := protobuf.Decode(body)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			var want any
			if err := json.Unmarshal(js, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got[tc.field], want) {
				t.Errorf("%s = %#v, want %#v", tc.field, got[tc.field], want)
			}
		})
	}
}
