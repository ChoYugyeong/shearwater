package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"shearwater/internal/agent"
	"shearwater/internal/apis"
)

func TestFocusedDiagnosisReadsActualOwnedPodAndRejectsScopeChanges(t *testing.T) {
	o, c, deployment := focusFixture(t, "diagnose")
	o.Write = false // 진단에는 승인이나 쓰기 허용이 필요하지 않다.
	if len(availableMCPTools(o)) != 1 || availableMCPTools(o)[0].Name != "sw_target_diagnose" {
		t.Fatal("diagnosis must expose only one read-only tool")
	}
	for _, raw := range []string{`{"namespace":"default"}`, `{"resource":"pod/guessed"}`, `{"uid":"another"}`} {
		if _, err := mcpCall(o, "sw_target_diagnose", json.RawMessage(raw)); err == nil {
			t.Fatal("model changed scope", raw)
		}
	}
	if _, err := mcpCall(o, "sw_get", json.RawMessage(`{"resource":"pods"}`)); err == nil {
		t.Fatal("hidden generic read allowed")
	}
	owner := func(kind, name, uid string) []any {
		return []any{map[string]any{"apiVersion": "apps/v1", "kind": kind, "name": name, "uid": uid, "controller": true}}
	}
	meta := deployment["metadata"].(map[string]any)
	spec := deployment["spec"].(map[string]any)
	rs := focusObject(t, c, "POST", "/apis/apps/v1/namespaces/bench/replicasets", map[string]any{
		"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": map[string]any{"name": "web-rs", "namespace": "bench", "ownerReferences": owner("Deployment", "web", meta["uid"].(string))},
		"spec": map[string]any{"replicas": 1, "selector": spec["selector"], "template": spec["template"]}})
	pod := focusObject(t, c, "POST", "/api/v1/namespaces/bench/pods", map[string]any{
		"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "web-rs-pod", "namespace": "bench", "ownerReferences": owner("ReplicaSet", "web-rs", rs["metadata"].(map[string]any)["uid"].(string))},
		"spec": spec["template"].(map[string]any)["spec"]})
	pod["status"] = map[string]any{"phase": "Pending", "containerStatuses": []any{map[string]any{"name": "app", "ready": false, "restartCount": 0,
		"state": map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff", "message": "registry: image does not exist"}}}}}
	focusObject(t, c, "PUT", "/api/v1/namespaces/bench/pods/web-rs-pod/status", pod)
	text, err := mcpCall(o, "sw_target_diagnose", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "ImagePullBackOff") || strings.Contains(text, "PRIVATE-") || !strings.Contains(text, `"http":"not_checked"`) {
		t.Fatal(text)
	}
	var answer focusDiagnosis
	if err := json.Unmarshal(o.Focus.answer, &answer); err != nil || answer.OwnedPods != 1 || answer.Pods[0].Name != "web-rs-pod" {
		t.Fatal(answer, err)
	}
	// A later generation cannot be silently diagnosed as the original goal.
	spec["replicas"] = 2
	focusObject(t, c, "PUT", o.Focus.path(), deployment)
	text, err = mcpCall(o, "sw_target_diagnose", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(text, `"state":"stale"`) || strings.Contains(text, `"pods"`) {
		t.Fatal(text, err)
	}
}

func TestFocusedDiagnosisOwnershipPrivacyAndBoundedEvidence(t *testing.T) {
	controlled := true
	owner := func(kind, uid string) []apis.OwnerReference {
		return []apis.OwnerReference{{Kind: kind, UID: uid, Controller: &controlled}}
	}
	s := &focusSession{Namespace: "bench", goal: agent.ReadinessGoal{UID: "deployment"}}
	snapshot := agent.Snapshot{Revision: "8", ReplicaSets: []apis.ReplicaSet{
		{Metadata: apis.ObjectMeta{Namespace: "bench", UID: "owned", OwnerReferences: owner("Deployment", "deployment")}},
		{Metadata: apis.ObjectMeta{Namespace: "bench", UID: "impostor", OwnerReferences: owner("Deployment", "replacement")}},
	}}
	for n := range 10 {
		uid := "owned"
		if n == 9 {
			uid = "impostor"
		}
		p := apis.Pod{Metadata: apis.ObjectMeta{Name: fmt.Sprintf("pod-%d", n), Namespace: "bench", OwnerReferences: owner("ReplicaSet", uid)},
			Spec:   apis.PodSpec{Containers: []apis.Container{{Name: "app", Image: "nginx:missing"}}},
			Status: apis.PodStatus{Phase: "Pending", ContainerStatuses: []apis.ContainerStatus{{Name: "app", State: apis.ContainerState{Waiting: &apis.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: strings.Repeat("오류\x00", 1000) + " not found"}}}}}}
		if n == 0 {
			p.Status.Phase = "Running"
			p.Status.Conditions = []apis.PodCondition{{Type: "Ready", Status: "True"}}
		}
		snapshot.Pods = append(snapshot.Pods, p)
	}
	otherNamespace := snapshot.Pods[1]
	otherNamespace.Metadata.Namespace = "other"
	snapshot.Pods = append(snapshot.Pods, otherNamespace)
	got := focusEvidence(snapshot, s, focusStatus{HTTP: "not_checked"})
	b, err := json.Marshal(got)
	if err != nil || !json.Valid(b) || len(b) > 8<<10 || got.OwnedPods != 9 || got.ReadyPods != 1 || got.OmittedPods != 5 || len(got.Pods) != 4 {
		t.Fatal(got, len(b), err)
	}
	if strings.Contains(string(b), "pod-9") || got.Pods[0].Ready || !got.Pods[0].Containers[0].Truncated || !strings.HasSuffix(got.Pods[0].Containers[0].Message, "not found") {
		t.Fatal(string(b))
	}
}

func TestFocusedDiagnosisRejectsMixedRevisions(t *testing.T) {
	calls := 0
	get := func(_ context.Context, path string, out any) error {
		calls++
		if !strings.Contains(path, "/namespaces/bench/") {
			t.Fatal("read escaped namespace", path)
		}
		return json.Unmarshal([]byte(fmt.Sprintf(`{"metadata":{"resourceVersion":"%d"},"items":[]}`, calls)), out)
	}
	_, err := readFocusEvidence(context.Background(), get, &focusSession{Namespace: "bench", Target: "deploy/web"})
	if err == nil || !strings.Contains(err.Error(), "revision kept changing") || calls != inspectAttempts*2 {
		t.Fatal(calls, err)
	}
}
