package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"shearwater/internal/agent"
)

func TestPlanVerificationReadyAndIncompleteEvidence(t *testing.T) {
	c, root, _ := planTestServer(t)
	ctx := context.Background()
	p, err := preparePlan(ctx, c, root, "127.0.0.1:6443", plannedPod)
	if err != nil {
		t.Fatal(err)
	}
	id := planHash(p)
	exclusiveJSON(filepath.Join(root, id+".json"), p)
	exclusiveJSON(filepath.Join(root, id+".approved.json"), map[string]string{"plan": id})
	if err = executePlan(ctx, c, root, p.Listen, root, id, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	b, _, _, err := apiRequest(ctx, c, "GET", "/api/v1/namespaces/default/pods/planned", nil)
	if err != nil {
		t.Fatal(err)
	}
	obj, _ := objectMap(b)
	// This is API fixture evidence, not a running container. It tests the exact
	// readiness predicate and must not be reported as runtime verification.
	obj["status"] = map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	b, _ = json.Marshal(obj)
	if _, _, _, err = apiRequest(ctx, c, "PUT", "/api/v1/namespaces/default/pods/planned/status", b); err != nil {
		t.Fatal(err)
	}
	var original applyReceipt
	path := filepath.Join(root, id+".receipt.json")
	if err = readPrivateJSON(path, &original); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, state string
		fail        bool
		change      func(*applyReceipt)
	}{
		{"all ready", "met", false, func(r *applyReceipt) {}},
		{"legacy", "unknown", false, func(r *applyReceipt) { r.Identity = ""; r.Goals = nil }},
		{"unsupported target", "unknown", false, func(r *applyReceipt) { r.Unverified = []string{"/api/v1/namespaces/test"} }},
		{"partial", "unknown", false, func(r *applyReceipt) { r.State = "uncertain-or-partial" }},
		{"missing goal", "unknown", false, func(r *applyReceipt) { r.Goals = nil }},
		{"different connection", "", true, func(r *applyReceipt) { r.Identity = "other-server" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := original
			tt.change(&r)
			if err := atomicJSON(path, r); err != nil {
				t.Fatal(err)
			}
			v, err := verifyAppliedPlan(ctx, c, root, p.Listen, root, id)
			if (err != nil) != tt.fail || !tt.fail && v.State != tt.state {
				t.Fatal(v, err)
			}
		})
	}
}

func TestSingleGoalNamespace(t *testing.T) {
	if got := singleGoalNamespace(nil); got != "" {
		t.Fatalf("empty goals = %q", got)
	}
	same := []agent.ReadinessGoal{
		{Namespace: "team-a", Resource: "deploy/web", UID: "u1", Generation: 1},
		{Namespace: "team-a", Resource: "svc/web", UID: "u2", Generation: 1},
	}
	if got := singleGoalNamespace(same); got != "team-a" {
		t.Fatalf("same namespace goals = %q, want team-a", got)
	}
	mixed := append(same, agent.ReadinessGoal{Namespace: "team-b", Resource: "pod/p", UID: "u3", Generation: 1})
	if got := singleGoalNamespace(mixed); got != "" {
		t.Fatalf("mixed namespace goals = %q, want empty", got)
	}
	if got := singleGoalNamespace([]agent.ReadinessGoal{{Namespace: "Bad!"}}); got != "" {
		t.Fatalf("invalid namespace goal = %q, want empty", got)
	}
}
