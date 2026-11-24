package api

import (
	"strings"
	"testing"
)

func TestNetworkPolicyAPIAndApplyFlow(t *testing.T) {
	c := newClient(t)
	netpols := "/apis/networking.k8s.io/v1/namespaces/default/networkpolicies"

	// OpenAPI v3 must advertise patchNetworkPolicy with fieldValidation for kubectl apply -f.
	oa := c.want(c.do("GET", "/openapi/v3/apis/networking.k8s.io/v1", ""), 200)
	if !strings.Contains(oa.raw, `"patchNetworkPolicy"`) || !strings.Contains(oa.raw, `"fieldValidation"`) {
		t.Fatalf("OpenAPI v3 missing NetworkPolicy patch entry: %s", oa.raw)
	}

	// Create with fieldValidation=Strict (kubectl apply first pass).
	created := c.want(c.do("POST", netpols+"?fieldValidation=Strict", `{
		"apiVersion": "networking.k8s.io/v1",
		"kind": "NetworkPolicy",
		"metadata": {"name": "allow-web"},
		"spec": {
			"podSelector": {"matchLabels": {"app": "web"}},
			"ingress": [{"from": [{"podSelector": {"matchLabels": {"app": "front"}}}], "ports": [{"port": 80}]}]
		}
	}`), 201)
	if !strings.Contains(created.raw, `"policyTypes":["Ingress"]`) || !strings.Contains(created.raw, `"protocol":"TCP"`) {
		t.Fatalf("defaults were not applied on create: %s", created.raw)
	}

	// Strategic merge patch (kubectl apply second pass) adding egress rules.
	patched := c.want(c.do("PATCH", netpols+"/allow-web?fieldValidation=Strict", `{
		"spec": {
			"policyTypes": ["Ingress", "Egress"],
			"egress": [{"ports": [{"protocol": "UDP", "port": 53}]}]
		}
	}`, "Content-Type", strategicPatchType), 200)
	if !strings.Contains(patched.raw, `"Egress"`) || !strings.Contains(patched.raw, `"port":53`) {
		t.Fatalf("strategic merge patch did not update NetworkPolicy: %s", patched.raw)
	}

	// Invalid NetworkPolicy rejected with 422.
	c.want(c.do("POST", netpols, `{
		"metadata": {"name": "bad"},
		"spec": {"ingress": [{"from": [{"ipBlock": {"cidr": "10.0.0.0/24", "except": ["10.0.0.0/16"]}}]}]}
	}`), 422)

	// Delete succeeds.
	c.want(c.do("DELETE", netpols+"/allow-web", `{"propagationPolicy":"Background"}`), 200)
}
