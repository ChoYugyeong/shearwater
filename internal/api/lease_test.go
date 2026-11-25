package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shearwater/internal/operation"
)

func TestLeaseJSONAndTable(t *testing.T) {
	c := newClient(t)
	const leases = "/apis/coordination.k8s.io/v1/namespaces/default/leases"
	const body = `{"apiVersion":"coordination.k8s.io/v1","kind":"Lease","metadata":{"name":"example"},"spec":{"holderIdentity":"operator","leaseDurationSeconds":30,"renewTime":"2026-11-25T01:02:03.123456Z"}}`
	c.want(c.do("POST", leases, body), 201)
	tb := tableOf(t, c.want(c.do("GET", leases, "", "Accept", tableAccept), 200))
	if strings.Join(columnNames(tb), ",") != "Name,Holder,Age" || firstRow(t, tb)[1] != "operator" {
		t.Fatal(tb)
	}
	const path = leases + "/example"
	p := c.want(c.do("PATCH", path, `{"spec":{"holderIdentity":"next"}}`, "Content-Type", "application/merge-patch+json"), 200)
	if p.at("spec", "holderIdentity") != "next" || p.at("spec", "renewTime") != "2026-11-25T01:02:03.123456Z" {
		t.Fatal(p.raw)
	}
	c.want(c.do("DELETE", path, ""), 200)
	c.want(c.do("GET", path, ""), 404)
}

func TestLeaseConflictHTTPRetryAndHint(t *testing.T) {
	c := newClient(t)
	spec, err := json.Marshal(operation.Spec{Name: "web", Namespace: "default", Image: "nginx", Port: 80, Checks: []operation.Check{{Path: "/"}}, Allow: []string{"deploy"}, Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	create := func() string {
		return c.want(c.do("POST", operation.Prefix, string(spec)), 200).at("task", "operation").(string)
	}
	a, b := create(), create()
	c.want(c.do("POST", operation.Prefix+"/"+a, `{"action":"deploy"}`), 200)
	r := c.want(c.do("POST", operation.Prefix+"/"+b, `{"action":"deploy"}`), 409)
	message := r.at("message").(string)
	for _, want := range []string{"APP_LEASED retry=safe", "hint:", "task:" + a, "expires=", "deploy web to nginx"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q in %q", want, message)
		}
	}
}
