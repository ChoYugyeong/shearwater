package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"shearwater/internal/auth"
	"shearwater/internal/rbac"
	"shearwater/internal/store"
)

func TestSummarizeChange(t *testing.T) {
	cases := []struct {
		name, prev, data string
		secret           bool
		want             []string
		class            string
	}{
		{"replicas and image by container name",
			`{"metadata":{"resourceVersion":"4","generation":1},"spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"web","image":"nginx:1.26"}]}}}}`,
			`{"metadata":{"resourceVersion":"9","generation":2},"spec":{"replicas":3,"template":{"spec":{"containers":[{"name":"web","image":"nginx:1.27"}]}}}}`,
			false, []string{"spec.replicas 2→3", "spec.template.spec.containers[web].image nginx:1.26→nginx:1.27"}, ""},
		{"labels in brackets, added and removed keys",
			`{"metadata":{"labels":{"app":"web","old":"x"}}}`,
			`{"metadata":{"labels":{"app":"web","app.kubernetes.io/part-of":"shop"}}}`,
			false, []string{`+metadata.labels["app.kubernetes.io/part-of"]=shop`, "-metadata.labels.old"}, ""},
		{"status only is folded as status",
			`{"spec":{"replicas":1},"status":{"readyReplicas":0}}`,
			`{"spec":{"replicas":1},"status":{"readyReplicas":1}}`,
			false, []string{"status.readyReplicas 0→1"}, "status"},
		{"timestamps only are a heartbeat",
			`{"status":{"conditions":[{"type":"Ready","status":"True","lastHeartbeatTime":"2026-11-25T01:00:00Z"}]}}`,
			`{"status":{"conditions":[{"type":"Ready","status":"True","lastHeartbeatTime":"2026-11-25T01:00:10Z"}]}}`,
			false, nil, "heartbeat"},
		{"kubectl's applied copy is never shown",
			`{"metadata":{"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"p\":\"czE=\"}}"}},"data":{"p":"czE="}}`,
			`{"metadata":{"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"p\":\"czI=\"}}"}},"data":{"p":"czI="}}`,
			true, []string{"data.p changed"}, ""},
		{"a new secret key shows no value",
			`{"data":{}}`, `{"data":{"token":"c2VjcmV0"}}`,
			true, []string{"+data.token=(hidden)"}, ""},
		{"list length",
			`{"spec":{"ports":[{"port":80}]}}`, `{"spec":{"ports":[{"port":80},{"port":443}]}}`,
			false, []string{"spec.ports 1→2 items"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := store.Key{Group: "apps", Resource: "deployments", Namespace: "a", Name: "web"}
			if c.secret {
				k = store.Key{Resource: "secrets", Namespace: "a", Name: "s"}
			}
			got, class := summarizeChange(store.Entry{Change: store.Change{Key: k, Type: "MODIFIED", Prev: []byte(c.prev), Data: []byte(c.data)}})
			if strings.Join(got, "|") != strings.Join(c.want, "|") || class != c.class {
				t.Fatalf("got %q (%q), want %q (%q)", got, class, c.want, c.class)
			}
			if c.secret && strings.Contains(strings.Join(got, " "), "c2Vj") {
				t.Fatal("secret value leaked")
			}
		})
	}
	many := `{"a":1,"b":1,"c":1,"d":1,"e":1,"f":1,"g":1,"h":1}`
	got, _ := summarizeChange(store.Entry{Change: store.Change{Type: "MODIFIED", Prev: []byte(many),
		Data: []byte(strings.ReplaceAll(many, "1", "2"))}})
	if len(got) != maxChangeFields+1 || got[maxChangeFields] != "+2 more" {
		t.Fatalf("not capped: %q", got)
	}
	if f, class := summarizeChange(store.Entry{Change: store.Change{Type: "ADDED", Data: []byte(`{}`)}}); f != nil || class != "" {
		t.Fatalf("ADDED: %q %q", f, class)
	}
}

func changeList(t *testing.T, r reply) ChangeList {
	t.Helper()
	var l ChangeList
	if err := json.Unmarshal([]byte(r.raw), &l); err != nil {
		t.Fatalf("%v: %s", err, r.raw)
	}
	return l
}

func TestChangesEndpoint(t *testing.T) {
	c := newClient(t)
	st := c.srv.Store
	for _, err := range []error{st.InitActors(), st.InitCredentials(), rbac.Bootstrap(st)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	e := rbac.New(st)
	c.srv.Authorizer, c.srv.Admit = e, e.Admit
	c.srv.Auth = &auth.Authenticator{AdminToken: "secret", Credentials: st}
	token, id, hash, err := auth.NewToken(auth.KindUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutCredential(store.Credential{Kind: auth.KindUser, Name: "agent-a", ID: id, Hash: hash}); err != nil {
		t.Fatal(err)
	}
	start := c.rev()
	for _, ns := range []string{"team-a", "team-b"} {
		c.want(c.do("POST", "/api/v1/namespaces", `{"metadata":{"name":"`+ns+`"}}`, "User-Agent", "kubectl/v1.36.6 (linux/amd64)"), 201)
	}
	c.want(c.do("POST", "/apis/rbac.authorization.k8s.io/v1/namespaces/team-a/rolebindings",
		`{"metadata":{"name":"agent-a"},"roleRef":{"kind":"ClusterRole","name":"edit"},"subjects":[{"name":"agent-a","kind":"User"}]}`), 201)
	asAgent := func(method, path, body string) reply {
		return c.do(method, path, body, "Authorization", "Bearer "+token, "User-Agent", "kubectl/v1.36.6 (linux/amd64)")
	}
	const deploy = `{"metadata":{"name":"web"},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"web","image":"nginx:1.26"}]}}}}`
	c.want(asAgent("POST", "/apis/apps/v1/namespaces/team-a/deployments", deploy), 201)
	c.want(c.do("POST", "/apis/apps/v1/namespaces/team-b/deployments", deploy), 201)
	c.want(c.do("PATCH", "/apis/apps/v1/namespaces/team-a/deployments/web", `{"spec":{"replicas":3}}`,
		"Authorization", "Bearer "+token, "Content-Type", "application/merge-patch+json", "User-Agent", "kubectl/v1.36.6"), 200)
	c.want(c.do("PATCH", "/apis/apps/v1/namespaces/team-a/deployments/web/status", `{"status":{"observedGeneration":2}}`,
		"Content-Type", "application/merge-patch+json"), 200)

	// The administrator sees everything since start, with who did it.
	all := changeList(t, c.want(c.do("GET", fmt.Sprintf("%s?since=%d", ChangesPath, start), ""), 200))
	var lines []string
	for _, ch := range all.Changes {
		lines = append(lines, fmt.Sprintf("%s %s %s %s/%s %s", ch.Actor, ch.Via, ch.Type, ch.Namespace, ch.Name, strings.Join(ch.Fields, ";")))
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{
		"shearwater-admin kubectl ADDED /team-a",
		"agent-a kubectl ADDED team-a/web",
		"shearwater-admin api ADDED team-b/web",
		"agent-a kubectl MODIFIED team-a/web spec.replicas 1→3",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if all.Folded["status"] != 1 || all.Next != c.rev() || all.More {
		t.Fatalf("folded/next: %+v (rev %d)\n%s", all, c.rev(), text)
	}
	withStatus := changeList(t, c.want(c.do("GET", fmt.Sprintf("%s?since=%d&status=true", ChangesPath, start), ""), 200))
	if len(withStatus.Changes) != len(all.Changes)+1 {
		t.Fatalf("status=true: %d vs %d", len(withStatus.Changes), len(all.Changes))
	}

	// The agent sees only team-a, and nothing about team-b.
	mine := changeList(t, c.want(asAgent("GET", fmt.Sprintf("%s?since=%d", ChangesPath, start), ""), 200))
	for _, ch := range mine.Changes {
		if ch.Namespace != "team-a" {
			t.Fatalf("agent saw %+v", ch)
		}
	}
	if len(mine.Changes) != 2 {
		t.Fatalf("agent changes: %+v", mine.Changes)
	}
	byActor := changeList(t, c.want(c.do("GET", fmt.Sprintf("%s?since=%d&actor=agent-a", ChangesPath, start), ""), 200))
	if len(byActor.Changes) != 2 {
		t.Fatalf("actor filter: %+v", byActor.Changes)
	}

	// Paging: continue from next and get exactly the rest.
	first := changeList(t, c.want(c.do("GET", fmt.Sprintf("%s?since=%d&limit=2", ChangesPath, start), ""), 200))
	if len(first.Changes) != 2 || !first.More {
		t.Fatalf("page 1: %+v", first)
	}
	rest := changeList(t, c.want(c.do("GET", fmt.Sprintf("%s?since=%d", ChangesPath, first.Next), ""), 200))
	if len(first.Changes)+len(rest.Changes) != len(all.Changes) || rest.Changes[0].Rev <= first.Changes[1].Rev {
		t.Fatalf("page 2: %+v", rest)
	}
	// Without since: the newest ones.
	newest := changeList(t, c.want(c.do("GET", ChangesPath+"?limit=1", ""), 200))
	if len(newest.Changes) != 1 || newest.Changes[0].Rev != all.Changes[len(all.Changes)-1].Rev {
		t.Fatalf("newest: %+v", newest)
	}
	c.want(c.do("GET", ChangesPath+"?since=-1", ""), 400)
	c.want(c.do("GET", ChangesPath+"?limit=500", ""), 400)
	c.want(c.do("POST", ChangesPath, "{}"), 405)
}

func TestChangesEndpointCompacted(t *testing.T) {
	c := newClient(t)
	st := c.srv.Store
	st.KeepChanges, st.KeepFor, st.CompactEvery = 1, -time.Hour, 0
	for i := 0; i < 3; i++ {
		c.want(c.do("POST", pods, strings.Replace(podBody, `"p1"`, fmt.Sprintf(`"p%d"`, i), 1)), 201)
	}
	r := c.want(c.do("GET", ChangesPath+"?since=1", ""), 410)
	if !strings.Contains(r.raw, "COMPACTED retry=never") || !strings.Contains(r.raw, "read the current state again") {
		t.Fatal(r.raw)
	}
}
