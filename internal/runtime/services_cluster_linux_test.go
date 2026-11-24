package runtime

import (
	"net/netip"
	"strings"
	"testing"
)

func TestHostServiceTrafficToOtherNodesIsMasqueraded(t *testing.T) {
	k := &KernelServices{PodCIDR: netip.MustParsePrefix("10.244.1.0/24"), ServiceCIDR: netip.MustParsePrefix("10.96.0.0/16"),
		ClusterCIDR: netip.MustParsePrefix("10.244.0.0/16")}
	nat, _ := k.rules(nil, nil)
	want := "-A " + serviceSNAT + " -p tcp -d 10.244.0.0/16 -m addrtype --src-type LOCAL -m conntrack --ctorigdst 10.96.0.0/16 --ctstate DNAT -m comment --comment " + serviceMarker + " -j MASQUERADE"
	if !strings.Contains(nat, want) {
		t.Fatalf("missing host masquerade rule:\n%s", nat)
	}
	// iptables -S prints the matches in its own order and quotes the comment.
	row := "-A " + serviceSNAT + " -d 10.244.0.0/16 -p tcp -m addrtype --src-type LOCAL -m conntrack --ctorigdst 10.96.0.0/16 --ctstate DNAT -m comment --comment \"" + serviceMarker + "\" -j MASQUERADE"
	if !ownedServiceRule(row, serviceSNAT) {
		t.Fatalf("our own rule is not recognized: %s", row)
	}
	for _, foreign := range []string{
		strings.Replace(row, "--src-type LOCAL", "--src-type UNICAST", 1),
		strings.Replace(row, " -m addrtype --src-type LOCAL", " -m addrtype --dst-type LOCAL", 1),
	} {
		if ownedServiceRule(foreign, serviceSNAT) {
			t.Fatalf("a rule of another shape passed as ours: %s", foreign)
		}
	}
	hooks := k.hooks()
	if last := hooks[len(hooks)-1]; last.name != "POSTROUTING" || last.rules[0][1] != "10.244.0.0/16" {
		t.Fatalf("no POSTROUTING hook for the cluster range: %+v", last)
	}
	single := &KernelServices{PodCIDR: k.PodCIDR, ServiceCIDR: k.ServiceCIDR}
	if nat, _ := single.rules(nil, nil); strings.Contains(nat, "addrtype") || len(single.hooks()) != 4 {
		t.Fatal("a single server must keep its rules exactly as before")
	}
}
