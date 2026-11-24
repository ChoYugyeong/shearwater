package apis

import (
	"strings"
	"testing"
)

func TestNetworkPolicyDefaultsAndValidation(t *testing.T) {
	if Lookup("networking.k8s.io", "v1", "networkpolicies") == nil {
		t.Fatal("NetworkPolicy resource is not registered")
	}

	// Defaulting: no policyTypes, no egress -> [Ingress] only.
	denyIngress := decodeInto[NetworkPolicy](t, `{
		"apiVersion": "networking.k8s.io/v1",
		"kind": "NetworkPolicy",
		"metadata": {"name": "default-deny-ingress", "namespace": "default"},
		"spec": {"podSelector": {}}
	}`)
	DefaultNetworkPolicy(denyIngress)
	if errs := ValidateNetworkPolicy(denyIngress); len(errs) > 0 {
		t.Fatalf("default-deny-ingress failed validation:\n%s", reasons(errs))
	}
	if len(denyIngress.Spec.PolicyTypes) != 1 || denyIngress.Spec.PolicyTypes[0] != PolicyTypeIngress {
		t.Errorf("policyTypes = %v, want [Ingress]", denyIngress.Spec.PolicyTypes)
	}

	// Defaulting: no policyTypes, with egress -> [Ingress, Egress], and port protocol defaults to TCP.
	withEgress := decodeInto[NetworkPolicy](t, `{
		"apiVersion": "networking.k8s.io/v1",
		"kind": "NetworkPolicy",
		"metadata": {"name": "web-netpol", "namespace": "default"},
		"spec": {
			"podSelector": {"matchLabels": {"role": "db"}},
			"ingress": [{
				"from": [
					{"ipBlock": {"cidr": "172.17.0.0/16", "except": ["172.17.1.0/24"]}},
					{"namespaceSelector": {"matchLabels": {"project": "myproject"}}},
					{"podSelector": {"matchLabels": {"role": "frontend"}}},
					{"namespaceSelector": {"matchLabels": {"env": "prod"}}, "podSelector": {"matchLabels": {"app": "api"}}}
				],
				"ports": [
					{"port": 6379},
					{"protocol": "UDP", "port": "dns"},
					{"protocol": "TCP", "port": 32000, "endPort": 32768},
					{"protocol": "UDP"}
				]
			}],
			"egress": [{
				"to": [{"ipBlock": {"cidr": "10.0.0.0/24"}}],
				"ports": [{"port": 5978}]
			}]
		}
	}`)
	DefaultNetworkPolicy(withEgress)
	if errs := ValidateNetworkPolicy(withEgress); len(errs) > 0 {
		t.Fatalf("valid NetworkPolicy failed validation:\n%s", reasons(errs))
	}
	if len(withEgress.Spec.PolicyTypes) != 2 || withEgress.Spec.PolicyTypes[0] != PolicyTypeIngress || withEgress.Spec.PolicyTypes[1] != PolicyTypeEgress {
		t.Errorf("policyTypes = %v, want [Ingress Egress]", withEgress.Spec.PolicyTypes)
	}
	if withEgress.Spec.Ingress[0].Ports[0].Protocol != "TCP" {
		t.Errorf("ingress port protocol = %q, want TCP", withEgress.Spec.Ingress[0].Ports[0].Protocol)
	}
	if withEgress.Spec.Egress[0].Ports[0].Protocol != "TCP" {
		t.Errorf("egress port protocol = %q, want TCP", withEgress.Spec.Egress[0].Ports[0].Protocol)
	}

	// Explicit policyTypes: [Egress] is preserved even without egress rules (default deny egress).
	denyEgress := decodeInto[NetworkPolicy](t, `{
		"metadata": {"name": "default-deny-egress", "namespace": "default"},
		"spec": {"podSelector": {}, "policyTypes": ["Egress"]}
	}`)
	DefaultNetworkPolicy(denyEgress)
	if errs := ValidateNetworkPolicy(denyEgress); len(errs) > 0 {
		t.Fatalf("default-deny-egress failed validation:\n%s", reasons(errs))
	}
	if len(denyEgress.Spec.PolicyTypes) != 1 || denyEgress.Spec.PolicyTypes[0] != PolicyTypeEgress {
		t.Errorf("explicit policyTypes = %v, want [Egress]", denyEgress.Spec.PolicyTypes)
	}

	// Empty egress: [] without policyTypes defaults to [Ingress] only (Kubernetes behavior).
	emptyEgress := decodeInto[NetworkPolicy](t, `{
		"metadata": {"name": "empty-egress"},
		"spec": {"podSelector": {}, "egress": []}
	}`)
	DefaultNetworkPolicy(emptyEgress)
	if len(emptyEgress.Spec.PolicyTypes) != 1 || emptyEgress.Spec.PolicyTypes[0] != PolicyTypeIngress {
		t.Errorf("empty egress without policyTypes gave %v, want [Ingress]", emptyEgress.Spec.PolicyTypes)
	}

	bad := []struct{ name, body, want string }{
		{"bad policyType", `{"metadata":{"name":"x"},"spec":{"policyTypes":["Both"]}}`, "Unsupported value"},
		{"duplicate policyType", `{"metadata":{"name":"x"},"spec":{"policyTypes":["Ingress","Ingress"]}}`, "Duplicate value"},
		{"bad podSelector", `{"metadata":{"name":"x"},"spec":{"podSelector":{"matchExpressions":[{"key":"a","operator":"In"}]}}}`, "spec.podSelector"},
		{"empty peer", `{"metadata":{"name":"x"},"spec":{"ingress":[{"from":[{}]}]}}`, "must specify at least one"},
		{"ipBlock with podSelector", `{"metadata":{"name":"x"},"spec":{"ingress":[{"from":[{"ipBlock":{"cidr":"10.0.0.0/8"},"podSelector":{}}]}]}}`, "may not be combined"},
		{"ipBlock with namespaceSelector", `{"metadata":{"name":"x"},"spec":{"egress":[{"to":[{"ipBlock":{"cidr":"10.0.0.0/8"},"namespaceSelector":{}}]}]}}`, "may not be combined"},
		{"ipBlock missing cidr", `{"metadata":{"name":"x"},"spec":{"ingress":[{"from":[{"ipBlock":{}}]}]}}`, "ipBlock.cidr: Required"},
		{"ipBlock bad cidr", `{"metadata":{"name":"x"},"spec":{"ingress":[{"from":[{"ipBlock":{"cidr":"not-a-cidr"}}]}]}}`, "valid IPv4 CIDR"},
		{"ipBlock ipv6 cidr", `{"metadata":{"name":"x"},"spec":{"ingress":[{"from":[{"ipBlock":{"cidr":"fd00::/64"}}]}]}}`, "only IPv4 CIDRs"},
		{"ipBlock except outside cidr", `{"metadata":{"name":"x"},"spec":{"ingress":[{"from":[{"ipBlock":{"cidr":"172.17.0.0/16","except":["10.0.0.0/24"]}}]}]}}`, "strict subset"},
		{"ipBlock except equal to cidr", `{"metadata":{"name":"x"},"spec":{"ingress":[{"from":[{"ipBlock":{"cidr":"172.17.0.0/16","except":["172.17.0.0/16"]}}]}]}}`, "strict subset"},
		{"ipBlock except broader than cidr", `{"metadata":{"name":"x"},"spec":{"ingress":[{"from":[{"ipBlock":{"cidr":"172.17.1.0/24","except":["172.17.0.0/16"]}}]}]}}`, "strict subset"},
		{"sctp unsupported", `{"metadata":{"name":"x"},"spec":{"ingress":[{"ports":[{"protocol":"SCTP","port":80}]}]}}`, "only TCP and UDP"},
		{"bad protocol", `{"metadata":{"name":"x"},"spec":{"ingress":[{"ports":[{"protocol":"ICMP","port":80}]}]}}`, "Unsupported value"},
		{"port zero", `{"metadata":{"name":"x"},"spec":{"ingress":[{"ports":[{"port":0}]}]}}`, "between 1 and 65535"},
		{"port too high", `{"metadata":{"name":"x"},"spec":{"ingress":[{"ports":[{"port":70000}]}]}}`, "between 1 and 65535"},
		{"bad named port", `{"metadata":{"name":"x"},"spec":{"ingress":[{"ports":[{"port":"BAD_PORT"}]}]}}`, "lowercase RFC 1123 label"},
		{"endPort with named port", `{"metadata":{"name":"x"},"spec":{"ingress":[{"ports":[{"port":"http","endPort":8080}]}]}}`, "named port"},
		{"endPort without port", `{"metadata":{"name":"x"},"spec":{"egress":[{"ports":[{"endPort":8080}]}]}}`, "when `port` is not specified"},
		{"endPort less than port", `{"metadata":{"name":"x"},"spec":{"egress":[{"ports":[{"port":8080,"endPort":8000}]}]}}`, "greater than or equal"},
		{"endPort out of range", `{"metadata":{"name":"x"},"spec":{"egress":[{"ports":[{"port":8080,"endPort":70000}]}]}}`, "between 1 and 65535"},
	}
	for _, tc := range bad {
		np := decodeInto[NetworkPolicy](t, tc.body)
		DefaultNetworkPolicy(np)
		got := reasons(ValidateNetworkPolicy(np))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: want error containing %q, got:\n%s", tc.name, tc.want, got)
		}
	}
}
