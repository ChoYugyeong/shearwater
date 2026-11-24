package runtime

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNetpolRulesGolden(t *testing.T) {
	k := &KernelNetpol{PodCIDR: netip.MustParsePrefix("10.244.0.0/24")}
	plan := NetpolPlan{
		HasPolicies: true,
		Pods: []PodNetpol{
			{
				// Unsorted order on purpose to verify deterministic IP sorting.
				IP:              netip.MustParseAddr("10.244.0.5"),
				IngressIsolated: true, // default deny ingress
			},
			{
				IP:              netip.MustParseAddr("10.244.0.2"),
				IngressIsolated: true,
				Ingress: []NetpolRule{
					{Peer: netip.MustParsePrefix("10.244.0.3/32"), Protocol: "TCP", Port: 80},
					{Peer: netip.MustParsePrefix("172.17.0.0/24"), Protocol: "TCP", Port: 8000, EndPort: 8080},
					{Peer: netip.MustParsePrefix("0.0.0.0/0"), Protocol: "UDP"},
				},
				EgressIsolated: true,
				Egress: []NetpolRule{
					{Peer: netip.MustParsePrefix("0.0.0.0/0"), Protocol: "UDP", Port: 53},
					{Peer: netip.MustParsePrefix("10.244.1.4/32")},
				},
			},
		},
	}
	if err := k.validate(plan); err != nil {
		t.Fatalf("validate: %v", err)
	}
	got := k.rules(plan)
	want := strings.Join([]string{
		"*filter",
		":SHEARWATER-NETPOL - [0:0]",
		":SHEARWATER-NP-EGRESS - [0:0]",
		":SHEARWATER-NP-INGRESS - [0:0]",
		"-A SHEARWATER-NETPOL -m conntrack --ctstate RELATED,ESTABLISHED -m comment --comment shearwater:netpol:v1 -j RETURN",
		"-A SHEARWATER-NETPOL -s 10.244.0.0/24 -m comment --comment shearwater:netpol:v1 -j SHEARWATER-NP-EGRESS",
		"-A SHEARWATER-NETPOL -d 10.244.0.0/24 -m comment --comment shearwater:netpol:v1 -j SHEARWATER-NP-INGRESS",
		"-A SHEARWATER-NETPOL -m comment --comment shearwater:netpol:v1 -j RETURN",
		"-A SHEARWATER-NP-EGRESS -s 10.244.0.2/32 -p udp --dport 53 -m comment --comment shearwater:netpol:v1 -j RETURN",
		"-A SHEARWATER-NP-EGRESS -s 10.244.0.2/32 -d 10.244.1.4/32 -m comment --comment shearwater:netpol:v1 -j RETURN",
		"-A SHEARWATER-NP-EGRESS -s 10.244.0.2/32 -m comment --comment shearwater:netpol:v1 -j DROP",
		"-A SHEARWATER-NP-EGRESS -m comment --comment shearwater:netpol:v1 -j RETURN",
		"-A SHEARWATER-NP-INGRESS -s 10.244.0.3/32 -d 10.244.0.2/32 -p tcp --dport 80 -m comment --comment shearwater:netpol:v1 -j RETURN",
		"-A SHEARWATER-NP-INGRESS -s 172.17.0.0/24 -d 10.244.0.2/32 -p tcp --dport 8000:8080 -m comment --comment shearwater:netpol:v1 -j RETURN",
		"-A SHEARWATER-NP-INGRESS -d 10.244.0.2/32 -p udp -m comment --comment shearwater:netpol:v1 -j RETURN",
		"-A SHEARWATER-NP-INGRESS -d 10.244.0.2/32 -m comment --comment shearwater:netpol:v1 -j DROP",
		"-A SHEARWATER-NP-INGRESS -d 10.244.0.5/32 -m comment --comment shearwater:netpol:v1 -j DROP",
		"-A SHEARWATER-NP-INGRESS -m comment --comment shearwater:netpol:v1 -j RETURN",
		"COMMIT",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("generated rules mismatch:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestNetpolRulesEmptyPodsWithPoliciesGolden(t *testing.T) {
	k := &KernelNetpol{PodCIDR: netip.MustParsePrefix("10.244.1.0/24")}
	got := k.rules(NetpolPlan{HasPolicies: true})
	want := strings.Join([]string{
		"*filter",
		":SHEARWATER-NETPOL - [0:0]",
		":SHEARWATER-NP-EGRESS - [0:0]",
		":SHEARWATER-NP-INGRESS - [0:0]",
		"-A SHEARWATER-NETPOL -m conntrack --ctstate RELATED,ESTABLISHED -m comment --comment shearwater:netpol:v1 -j RETURN",
		"-A SHEARWATER-NETPOL -s 10.244.1.0/24 -m comment --comment shearwater:netpol:v1 -j SHEARWATER-NP-EGRESS",
		"-A SHEARWATER-NETPOL -d 10.244.1.0/24 -m comment --comment shearwater:netpol:v1 -j SHEARWATER-NP-INGRESS",
		"-A SHEARWATER-NETPOL -m comment --comment shearwater:netpol:v1 -j RETURN",
		"-A SHEARWATER-NP-EGRESS -m comment --comment shearwater:netpol:v1 -j RETURN",
		"-A SHEARWATER-NP-INGRESS -m comment --comment shearwater:netpol:v1 -j RETURN",
		"COMMIT",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("generated rules mismatch:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestNetpolRulesRecognizeOnlyOwnedShapes(t *testing.T) {
	k := &KernelNetpol{PodCIDR: netip.MustParsePrefix("10.244.0.0/24")}
	plan := NetpolPlan{
		HasPolicies: true,
		Pods: []PodNetpol{
			{
				IP:              netip.MustParseAddr("10.244.0.2"),
				IngressIsolated: true,
				Ingress: []NetpolRule{
					{Peer: netip.MustParsePrefix("10.244.0.3/32"), Protocol: "TCP", Port: 80},
					{Peer: netip.MustParsePrefix("10.244.1.0/24"), Protocol: "TCP", Port: 8000, EndPort: 8080},
					{Peer: netip.MustParsePrefix("0.0.0.0/0"), Protocol: "UDP", Port: 53},
					{Peer: netip.MustParsePrefix("0.0.0.0/0")},
				},
				EgressIsolated: true,
				Egress: []NetpolRule{
					{Peer: netip.MustParsePrefix("0.0.0.0/0"), Protocol: "UDP", Port: 53},
					{Peer: netip.MustParsePrefix("192.0.2.0/24"), Protocol: "TCP"},
				},
			},
		},
	}
	script := k.rules(plan)
	for _, line := range strings.Split(script, "\n") {
		if !strings.HasPrefix(line, "-A ") {
			continue
		}
		chain := strings.Fields(line)[1]
		if !ownedNetpolRule(line, chain) {
			t.Fatalf("rejected generated rule: %s", line)
		}
		if ownedNetpolRule(line+" -i eth0", chain) {
			t.Fatalf("accepted foreign extra match on: %s", line)
		}
	}

	// Normalized forms that iptables -S outputs back (-m tcp / -m udp, quoted comment).
	normalized := []struct{ chain, line string }{
		{netpolIngress, `-A SHEARWATER-NP-INGRESS -s 10.244.0.3/32 -d 10.244.0.2/32 -p tcp -m tcp --dport 80 -m comment --comment "shearwater:netpol:v1" -j RETURN`},
		{netpolEgress, `-A SHEARWATER-NP-EGRESS -s 10.244.0.2/32 -p udp -m udp --dport 53 -m comment --comment "shearwater:netpol:v1" -j RETURN`},
		{netpolChain, `-A SHEARWATER-NETPOL -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment shearwater:netpol:v1 -j RETURN`},
	}
	for _, tc := range normalized {
		if !ownedNetpolRule(tc.line, tc.chain) {
			t.Errorf("rejected iptables -S normalized rule: %s", tc.line)
		}
	}

	// Foreign rules must be rejected.
	foreign := []struct{ chain, line string }{
		{netpolChain, "-A SHEARWATER-NETPOL -m comment --comment shearwater:netpol:v1 -j DROP"},
		{netpolChain, "-A SHEARWATER-NETPOL -s 10.244.0.0/24 -m comment --comment foreign -j SHEARWATER-NP-EGRESS"},
		{netpolChain, "-A SHEARWATER-NETPOL -m conntrack --ctstate NEW -m comment --comment shearwater:netpol:v1 -j RETURN"},
		{netpolEgress, "-A SHEARWATER-NP-EGRESS -s 10.244.0.0/24 -m comment --comment shearwater:netpol:v1 -j DROP"},
		{netpolEgress, "-A SHEARWATER-NP-EGRESS -s 10.244.0.2/32 -p icmp -m comment --comment shearwater:netpol:v1 -j RETURN"},
		{netpolEgress, "-A SHEARWATER-NP-EGRESS -s 10.244.0.2/32 -p tcp --dport 9000:8000 -m comment --comment shearwater:netpol:v1 -j RETURN"},
		{netpolEgress, "-A SHEARWATER-NP-EGRESS -s 10.244.0.2/32 --dport 80 -m comment --comment shearwater:netpol:v1 -j RETURN"},
		{netpolIngress, "-A SHEARWATER-NP-INGRESS -d 10.244.0.2/32 -m comment --comment shearwater:netpol:v1 -j ACCEPT"},
		{netpolIngress, "-A SHEARWATER-NP-INGRESS -d 10.244.0.2/32 -p tcp -m udp --dport 80 -m comment --comment shearwater:netpol:v1 -j RETURN"},
	}
	for _, tc := range foreign {
		if ownedNetpolRule(tc.line, tc.chain) {
			t.Errorf("adopted foreign rule: %s", tc.line)
		}
	}
}

func TestNetpolValidate(t *testing.T) {
	k := &KernelNetpol{PodCIDR: netip.MustParsePrefix("10.244.0.0/24")}
	bad := []NetpolPlan{
		{HasPolicies: true, Pods: []PodNetpol{{IP: netip.MustParseAddr("10.244.1.2")}}},
		{HasPolicies: true, Pods: []PodNetpol{{IP: netip.MustParseAddr("10.244.0.2")}, {IP: netip.MustParseAddr("10.244.0.2")}}},
		{HasPolicies: true, Pods: []PodNetpol{{IP: netip.MustParseAddr("10.244.0.2"), Ingress: []NetpolRule{{Peer: netip.MustParsePrefix("10.0.0.0/8"), Protocol: "SCTP"}}}}},
		{HasPolicies: true, Pods: []PodNetpol{{IP: netip.MustParseAddr("10.244.0.2"), Ingress: []NetpolRule{{Peer: netip.MustParsePrefix("10.0.0.0/8"), Port: 80}}}}},
		{HasPolicies: true, Pods: []PodNetpol{{IP: netip.MustParseAddr("10.244.0.2"), Egress: []NetpolRule{{Peer: netip.MustParsePrefix("10.0.0.0/8"), Protocol: "TCP", Port: 90, EndPort: 80}}}}},
	}
	for i, p := range bad {
		if err := k.validate(p); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

func TestEnsureBridgeNetfilter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge-nf-call-iptables")

	// Missing sysctl triggers modprobe, which creates the file with "0", then we write "1\n".
	probed := false
	k := &KernelNetpol{
		PodCIDR:    netip.MustParsePrefix("10.244.0.0/24"),
		sysctlPath: path,
		modprobe: func(context.Context) error {
			probed = true
			return os.WriteFile(path, []byte("0\n"), 0o644)
		},
	}
	if err := k.ensureBridgeNetfilter(t.Context()); err != nil {
		t.Fatalf("ensureBridgeNetfilter: %v", err)
	}
	if !probed {
		t.Fatal("expected modprobe to run when sysctl file is missing")
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("sysctl content = %q (err=%v), want 1", string(b), err)
	}

	// Already "1": modprobe is NOT called.
	probed = false
	if err := k.ensureBridgeNetfilter(t.Context()); err != nil {
		t.Fatalf("second ensureBridgeNetfilter: %v", err)
	}
	if probed {
		t.Fatal("modprobe must not run when sysctl file already exists")
	}

	// Modprobe failure reports a clear error.
	missingPath := filepath.Join(dir, "missing")
	kFail := &KernelNetpol{
		PodCIDR:    netip.MustParsePrefix("10.244.0.0/24"),
		sysctlPath: missingPath,
		modprobe:   func(context.Context) error { return errors.New("module not found") },
	}
	if err := kFail.ensureBridgeNetfilter(t.Context()); err == nil || !strings.Contains(err.Error(), "br_netfilter") {
		t.Fatalf("expected clear br_netfilter error, got %v", err)
	}
}
