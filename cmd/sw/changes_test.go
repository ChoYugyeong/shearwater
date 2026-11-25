package main

import (
	"bytes"
	"strings"
	"testing"

	"shearwater/internal/api"
)

func TestChangesQueryPath(t *testing.T) {
	if p := (changesQuery{Since: -1}).path(); p != api.ChangesPath {
		t.Fatal(p)
	}
	p := changesQuery{Since: 0, Namespace: "team-a", Actor: "system:node:w1", Limit: 10, Status: true}.path()
	for _, want := range []string{"since=0", "namespace=team-a", "actor=system%3Anode%3Aw1", "limit=10", "status=true"} {
		if !strings.Contains(p, want) {
			t.Fatalf("%s lacks %s", p, want)
		}
	}
}

func TestPrintChanges(t *testing.T) {
	var out bytes.Buffer
	err := printChanges(&out, api.ChangeList{Next: 42, More: true, Folded: map[string]int{"status": 3, "heartbeat": 1},
		Changes: []api.ChangeItem{
			{Rev: 40, At: "2026-11-25T10:00:00Z", Actor: "agent-a", Via: "kubectl", Type: "MODIFIED", Kind: "Deployment",
				Namespace: "team-a", Name: "web", Fields: []string{"spec.replicas 1→3"}},
			{Rev: 41, At: "2026-11-25T10:00:01Z", Actor: "system", Type: "ADDED", Kind: "Namespace", Name: "team-c"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"REV", "agent-a", "Deployment team-a/web", "spec.replicas 1→3", "Namespace team-c",
		"Folded: 3 status, 1 heartbeat", "Next: sw changes --since 42 (more may follow)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	out.Reset()
	if err := printChanges(&out, api.ChangeList{Next: 7}); err != nil || !strings.Contains(out.String(), "No changes to show.") {
		t.Fatal(out.String(), err)
	}
}

func TestChangesToolIsReadOnly(t *testing.T) {
	for _, tool := range mcpTools(false) {
		if tool.Name == "sw_changes" {
			if a, _ := tool.Annotations.(map[string]any); a["readOnlyHint"] != true {
				t.Fatalf("%+v", tool)
			}
			return
		}
	}
	t.Fatal("sw_changes is not offered")
}
