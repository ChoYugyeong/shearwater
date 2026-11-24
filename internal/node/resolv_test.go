package node

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shearwater/internal/apis"
)

func TestRenderResolvConfByPolicy(t *testing.T) {
	gw := netip.MustParseAddr("10.244.3.1")
	host := []byte("# host\nnameserver 1.1.1.1\nsearch corp.example\noptions edns0\n")

	// Default (unset) and ClusterFirst point to the node gateway with cluster search list and ndots:5.
	for _, policy := range []string{"", "ClusterFirst", "ClusterFirstWithHostNet"} {
		pod := &apis.Pod{
			Metadata: apis.ObjectMeta{Namespace: "team-a", Name: "web"},
			Spec:     apis.PodSpec{DNSPolicy: policy},
		}
		got := string(renderResolvConf(pod, gw, host))
		for _, want := range []string{
			"nameserver 10.244.3.1\n",
			"search team-a.svc.cluster.local svc.cluster.local cluster.local\n",
			"options ndots:5\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("policy %q: missing %q in:\n%s", policy, want, got)
			}
		}
	}

	// Default policy without dnsConfig returns host resolv.conf verbatim.
	podDef := &apis.Pod{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "p"},
		Spec:     apis.PodSpec{DNSPolicy: "Default"},
	}
	if got := string(renderResolvConf(podDef, gw, host)); got != string(host) {
		t.Errorf("Default policy must copy host resolv.conf, got:\n%s", got)
	}

	// None policy uses only dnsConfig.
	two := "2"
	podNone := &apis.Pod{
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "p"},
		Spec: apis.PodSpec{
			DNSPolicy: "None",
			DNSConfig: &apis.PodDNSConfig{
				Nameservers: []string{"9.9.9.9"},
				Searches:    []string{"custom.local"},
				Options: []apis.PodDNSConfigOption{
					{Name: "ndots", Value: &two},
					{Name: "single-request-reopen"},
				},
			},
		},
	}
	gotNone := string(renderResolvConf(podNone, gw, host))
	if strings.Contains(gotNone, "10.244.3.1") || strings.Contains(gotNone, "cluster.local") {
		t.Errorf("None policy leaked cluster defaults:\n%s", gotNone)
	}
	for _, want := range []string{"nameserver 9.9.9.9\n", "search custom.local\n", "options ndots:2 single-request-reopen\n"} {
		if !strings.Contains(gotNone, want) {
			t.Errorf("None policy missing %q:\n%s", want, gotNone)
		}
	}

	// ClusterFirst + dnsConfig merges and overrides ndots.
	three := "3"
	podMerged := &apis.Pod{
		Metadata: apis.ObjectMeta{Namespace: "prod", Name: "p"},
		Spec: apis.PodSpec{
			DNSPolicy: "ClusterFirst",
			DNSConfig: &apis.PodDNSConfig{
				Nameservers: []string{"8.8.8.8"},
				Searches:    []string{"extra.internal"},
				Options:     []apis.PodDNSConfigOption{{Name: "ndots", Value: &three}, {Name: "edns0"}},
			},
		},
	}
	gotMerged := string(renderResolvConf(podMerged, gw, host))
	for _, want := range []string{
		"nameserver 10.244.3.1\n",
		"nameserver 8.8.8.8\n",
		"search prod.svc.cluster.local svc.cluster.local cluster.local extra.internal\n",
		"options ndots:3 edns0\n",
	} {
		if !strings.Contains(gotMerged, want) {
			t.Errorf("ClusterFirst+dnsConfig missing %q:\n%s", want, gotMerged)
		}
	}
}

func TestEnsurePodFilesWritesResolvOnceUsingNodePodCIDR(t *testing.T) {
	dir := t.TempDir()
	agent := &Agent{
		DataDir: dir,
		PodCIDR: netip.MustParsePrefix("10.244.7.0/24"),
	}
	pod := &apis.Pod{
		Metadata: apis.ObjectMeta{Namespace: "shop", Name: "web", UID: "11111111-2222-4333-8444-555555555555"},
		Spec:     apis.PodSpec{DNSPolicy: "ClusterFirst"},
	}
	if _, err := agent.ensurePodFiles(pod, "10.244.7.5"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "pods", pod.Metadata.UID, "resolv.conf")
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "nameserver 10.244.7.1\n") || !strings.Contains(string(got), "search shop.svc.cluster.local") {
		t.Fatalf("unexpected resolv.conf:\n%s", got)
	}
	// Existing resolv.conf must not be overwritten on a later reconcile.
	custom := []byte("nameserver 10.0.0.1\n")
	if err := os.WriteFile(p, custom, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ensurePodFiles(pod, "10.244.7.5"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p)
	if string(after) != string(custom) {
		t.Fatalf("existing resolv.conf was overwritten:\n%s", after)
	}
}
