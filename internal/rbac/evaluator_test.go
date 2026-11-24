package rbac

import (
	"testing"

	"shearwater/internal/apis"
	"shearwater/internal/auth"
)

func TestRuleMatching(t *testing.T) {
	r := apis.PolicyRule{Verbs: []string{"*"}, APIGroups: []string{""}, Resources: []string{"*/log"}, ResourceNames: []string{"web"}}
	a := auth.Attributes{ResourceRequest: true, Verb: "get", Resource: "pods", Subresource: "log", Name: "web"}
	if !permits(r, a) {
		t.Fatal("named pod logs should match")
	}
	a.Name = "db"
	if permits(r, a) {
		t.Fatal("matched the wrong pod")
	}
	a.Name, a.Subresource = "web", ""
	if permits(r, a) {
		t.Fatal("a subresource grant matched its parent")
	}
	r.Resources = []string{"*"}
	a.APIGroup = "apps"
	if permits(r, a) {
		t.Fatal("resource wildcard crossed API groups")
	}
	r.APIGroups = []string{"*"}
	if !permits(r, a) {
		t.Fatal("wildcard did not match")
	}

	urls := apis.PolicyRule{Verbs: []string{"get"}, NonResourceURLs: []string{"/healthz", "/openapi/*"}}
	for path, want := range map[string]bool{"/healthz": true, "/healthz/etcd": false, "/openapi/v3": true, "/apis": false} {
		if permits(urls, auth.Attributes{Verb: "get", Path: path}) != want {
			t.Error(path)
		}
	}
}

func TestGroupAndNamespaceScopes(t *testing.T) {
	idx := &index{grants: map[subjectKey][]apis.PolicyRule{}}
	read := apis.PolicyRule{Verbs: []string{"list"}, APIGroups: []string{""}, Resources: []string{"pods"}}
	idx.grants[subjectKey{"team-a", "Group", "engineering"}] = []apis.PolicyRule{read}
	id := auth.Identity{Name: "alice", Groups: []string{"engineering"}}
	a := auth.Attributes{ResourceRequest: true, Verb: "list", Resource: "pods"}
	for ns, want := range map[string]bool{"team-a": true, "team-b": false, "": false} {
		if allowed(idx.rules(id, ns), a) != want {
			t.Error(ns)
		}
	}
	idx.grants[subjectKey{"", "Group", "engineering"}] = []apis.PolicyRule{read}
	if !allowed(idx.rules(id, ""), a) {
		t.Fatal("cluster binding did not authorize cluster-wide list")
	}
}

func TestRuleCoverage(t *testing.T) {
	owned := []apis.PolicyRule{
		{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"pods"}, ResourceNames: []string{"one"}},
		{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"pods"}, ResourceNames: []string{"two"}},
	}
	request := []apis.PolicyRule{{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"pods"}, ResourceNames: []string{"one", "two"}}}
	if !covers(owned, request) {
		t.Fatal("individual name grants should cover the union")
	}
	request[0].ResourceNames = nil
	if covers(owned, request) {
		t.Fatal("named grants covered all names")
	}
	request[0].Resources = []string{"*"}
	if covers(owned, request) {
		t.Fatal("concrete grants covered wildcard")
	}
}
