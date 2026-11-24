package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shearwater/internal/auth"
	"shearwater/internal/protobuf"
	"shearwater/internal/rbac"
	"shearwater/internal/store"
)

// Exercise the same content negotiation and routing as the typed kubectl client.
func TestKubectlAccessRequests(t *testing.T) {
	c := newClient(t)
	e := rbac.New(c.srv.Store)
	c.srv.Authorizer, c.srv.Admit = e, e.Admit
	c.want(c.do("POST", "/api/v1/namespaces", `{"metadata":{"name":"team-a"}}`), 201)
	const rb = "/apis/rbac.authorization.k8s.io/v1/"
	const az = "/apis/authorization.k8s.io/v1/"
	for _, tc := range []struct{ prefix, resource string }{
		{rb + "namespaces/team-a/", "roles"},
		{rb, "clusterroles"},
		{rb + "namespaces/team-a/", "rolebindings"},
		{rb, "clusterrolebindings"},
		{"/api/v1/namespaces/team-a/", "resourcequotas"},
		{az, "selfsubjectaccessreviews"},
		{az, "selfsubjectrulesreviews"},
		{"/apis/authentication.k8s.io/v1/", "selfsubjectreviews"},
	} {
		t.Run(tc.resource, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", "protobuf", "testdata", "access", tc.resource+".bin"))
			if err != nil {
				t.Fatal(err)
			}
			r := c.do("POST", tc.prefix+tc.resource+"?fieldValidation=Strict", string(body),
				"Content-Type", protobuf.ContentType, "Accept", protobuf.ContentType+",application/json")
			if r.code != 201 {
				t.Fatalf("HTTP %d: %s", r.code, r.raw)
			}
		})
	}
}

func TestTenantAuthorizationAndReviews(t *testing.T) {
	c := newClient(t)
	st := c.srv.Store
	if err := st.InitCredentials(); err != nil {
		t.Fatal(err)
	}
	if err := rbac.Bootstrap(st); err != nil {
		t.Fatal(err)
	}
	e := rbac.New(st)
	c.srv.Authorizer, c.srv.Admit = e, e.Admit
	c.srv.Auth = &auth.Authenticator{AdminToken: "secret", Credentials: st}
	token, id, hash, err := auth.NewToken(auth.KindUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutCredential(store.Credential{Kind: auth.KindUser, Name: "alice", ID: id, Hash: hash}); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"team-a", "team-b"} {
		c.want(c.do("POST", "/api/v1/namespaces", `{"metadata":{"name":"`+ns+`"}}`), 201)
	}
	const bindings = "/apis/rbac.authorization.k8s.io/v1/namespaces/team-a/rolebindings"
	c.want(c.do("POST", bindings, `{"metadata":{"name":"alice"},"roleRef":{"kind":"ClusterRole","name":"admin"},"subjects":[{"name":"alice","kind":"User"}]}`), 201)
	asAlice := func(method, path, body string) reply {
		return c.do(method, path, body, "Authorization", "Bearer "+token)
	}
	c.want(asAlice("GET", "/api/v1/namespaces/team-a/pods", ""), 200)
	denied := c.want(asAlice("GET", "/api/v1/namespaces/team-b/pods", ""), 403)
	if !strings.Contains(denied.at("message").(string), `User "alice" cannot list resource "pods" in API group "" in the namespace "team-b"`) {
		t.Fatal(denied.raw)
	}
	c.want(asAlice("GET", "/api/v1/pods", ""), 403)
	const roles = "/apis/rbac.authorization.k8s.io/v1/namespaces/team-a/roles"
	c.want(asAlice("POST", roles, `{"metadata":{"name":"reader"},"rules":[{"apiGroups":[""],"verbs":["get"],"resources":["pods"]}]}`), 201)
	c.want(asAlice("POST", roles, `{"metadata":{"name":"quota-writer"},"rules":[{"apiGroups":[""],"verbs":["update"],"resources":["resourcequotas"]}]}`), 403)
	c.want(asAlice("POST", bindings, `{"metadata":{"name":"bad"},"roleRef":{"kind":"ClusterRole","name":"cluster-admin"}}`), 403)
	c.want(asAlice("POST", bindings, `{"metadata":{"name":"ok"},"roleRef":{"kind":"Role","name":"reader"}}`), 201)
	c.want(c.do("POST", roles, `{"metadata":{"name":"delegate"},"rules":[{"apiGroups":["rbac.authorization.k8s.io"],"resources":["roles"],"verbs":["escalate"],"resourceNames":["special"]},{"apiGroups":["rbac.authorization.k8s.io"],"resources":["clusterroles"],"verbs":["bind"],"resourceNames":["cluster-admin"]}]}`), 201)
	c.want(c.do("POST", bindings, `{"metadata":{"name":"delegate-alice"},"roleRef":{"kind":"Role","name":"delegate"},"subjects":[{"kind":"User","name":"alice"}]}`), 201)
	c.want(asAlice("POST", roles, `{"metadata":{"name":"special"},"rules":[{"apiGroups":[""],"resources":["resourcequotas"],"verbs":["update"]}]}`), 201)
	c.want(asAlice("POST", bindings, `{"metadata":{"name":"delegated"},"roleRef":{"kind":"ClusterRole","name":"cluster-admin"}}`), 201)
	c.want(c.do("PATCH", bindings+"/ok", `{"roleRef":{"name":"admin","kind":"ClusterRole"}}`,
		"Authorization", "Bearer "+token, "Content-Type", "application/merge-patch+json"), 422)
	const reviews = "/apis/authorization.k8s.io/v1/"
	before := c.rev()
	for _, ns := range []string{"team-a", "team-b"} {
		r := c.want(asAlice("POST", reviews+"selfsubjectaccessreviews", `{"spec":{"resourceAttributes":{"verb":"list","resource":"pods","namespace":"`+ns+`"}}}`), 201)
		if r.at("status", "allowed") != (ns == "team-a") {
			t.Fatal(r.raw)
		}
	}
	who := c.want(asAlice("POST", "/apis/authentication.k8s.io/v1/selfsubjectreviews", `{}`), 201)
	if who.at("status", "userInfo", "username") != "alice" {
		t.Fatal(who.raw)
	}
	rules := c.want(asAlice("POST", reviews+"selfsubjectrulesreviews", `{"spec":{"namespace":"team-a"}}`), 201)
	if rules.at("status", "incomplete") != false || !strings.Contains(rules.raw, `"pods/log"`) {
		t.Fatal(rules.raw)
	}
	if c.rev() != before {
		t.Fatal("reviews changed the store revision")
	}
	c.want(c.do("DELETE", bindings+"/alice", ""), 200)
	c.want(asAlice("GET", "/api/v1/namespaces/team-a/pods", ""), 403)
}
