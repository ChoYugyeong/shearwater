package runtime

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestServiceRulesRecognizeOnlyOwnedShapes(t *testing.T) {
	k := &KernelServices{PodCIDR: netip.MustParsePrefix("10.244.0.0/24"), ServiceCIDR: netip.MustParsePrefix("10.96.0.0/16")}
	uid := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	routes := []ServiceRoute{{UID: uid, Address: netip.MustParseAddrPort("10.96.0.1:80"), Backends: []ServiceBackend{
		{UID: uid, Address: netip.MustParseAddrPort("10.244.0.2:8080")}, {UID: uid, Address: netip.MustParseAddrPort("10.244.0.3:8080")},
	}}}
	if err := k.validate(routes); err != nil {
		t.Fatal(err)
	}
	nat, filter := k.rules(routes, nil)
	for _, line := range strings.Split(nat+filter, "\n") {
		if !strings.HasPrefix(line, "-A ") {
			continue
		}
		chain := strings.Fields(line)[1]
		if !ownedServiceRule(line, chain) {
			t.Fatalf("rejected generated rule: %s", line)
		}
		if ownedServiceRule(line+" -s 192.0.2.1/32", chain) {
			t.Fatal("accepted a foreign extra match")
		}
	}
	for _, line := range []string{
		"-A " + serviceNAT + " -m comment --comment " + serviceMarker + " -j DROP",
		"-A " + serviceNAT + " -m comment --comment " + serviceMarker + " -m unknown --x y -j RETURN",
		"-A " + serviceNAT + " -d 10.96.0.1/32 -p tcp --dport 80 -m comment --comment shearwater:service:not-a-uid -j RETURN",
	} {
		if ownedServiceRule(line, serviceNAT) {
			t.Fatalf("adopted foreign rule: %s", line)
		}
	}
	paused, _ := k.rules(routes, map[netip.Addr]bool{routes[0].Address.Addr(): true})
	if strings.Contains(paused, "shearwater:backend:") || !strings.Contains(paused, "shearwater:service:"+uid) {
		t.Fatal("fence lost ownership or admitted new traffic")
	}
}

// This privileged test always unshares a private network BEFORE touching any
// address or rule. Opt-in is explicit; no live Shearwater node is needed.
func TestKernelServicesProtectForeignRulesAndRoutes(t *testing.T) {
	if os.Getenv("SW_TEST_SERVICE_NETNS") != "1" {
		t.Skip("set SW_TEST_SERVICE_NETNS=1 with root in isolated QA")
	}
	ctx := t.Context()
	err := onLockedThread(func() error {
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			return err
		}
		k := &KernelServices{PodCIDR: netip.MustParsePrefix("10.244.0.0/24"), ServiceCIDR: netip.MustParsePrefix("10.96.0.0/16")}
		uid := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		routes := []ServiceRoute{{UID: uid, Address: netip.MustParseAddrPort("10.96.20.1:80"), Backends: []ServiceBackend{{UID: uid, Address: netip.MustParseAddrPort("10.244.0.2:8080")}}}}
		if _, err := iptablesOutput(ctx, "-t", "nat", "-N", serviceNAT); err != nil {
			return err
		}
		if k.Reconcile(ctx, routes) == nil {
			return fmt.Errorf("adopted an empty foreign chain")
		}
		if _, err := iptablesOutput(ctx, "-t", "nat", "-X", serviceNAT); err != nil {
			return err
		}
		if _, err := iptablesOutput(ctx, "-t", "nat", "-N", "SW-USER-SENTINEL"); err != nil {
			return err
		}
		if _, err := iptablesOutput(ctx, "-t", "nat", "-A", "SW-USER-SENTINEL", "-j", "RETURN"); err != nil {
			return err
		}
		if _, err := iptablesOutput(ctx, "-t", "nat", "-A", "OUTPUT", "-j", "SW-USER-SENTINEL"); err != nil {
			return err
		}
		before, err := iptablesOutput(ctx, "-t", "nat", "-S", "OUTPUT")
		if err != nil {
			return err
		}
		if err := k.Reconcile(ctx, routes); err != nil {
			return err
		}
		programmed, err := iptablesOutput(ctx, "-t", "nat", "-S", serviceNAT)
		if err != nil {
			return err
		}
		n := NewNet("/run/shearwater/unused-private-test", k.PodCIDR, k.ServiceCIDR)
		if err := n.CheckHostRanges(ctx); err != nil {
			return err
		}
		if err := n.CheckFirewall(ctx); err != nil {
			return err
		}
		after, err := iptablesOutput(ctx, "-t", "nat", "-S", serviceNAT)
		if err != nil {
			return err
		}
		if after != programmed {
			return fmt.Errorf("read-only host checks changed forwarding")
		}
		s, err := nlOpenContext(ctx)
		if err != nil {
			return err
		}
		defer s.Close()
		foreign, err := s.createOwnedLink("sw-test-foreign", "dummy", "test:foreign")
		if err != nil {
			return err
		}
		if err := s.linkSetUp(foreign.index); err != nil {
			return err
		}
		if err := s.addrAdd(foreign.index, netip.MustParsePrefix("10.96.20.254/24")); err != nil {
			return err
		}
		k.checked = time.Time{}
		if k.Reconcile(ctx, routes) == nil {
			return fmt.Errorf("ignored a new foreign route")
		}
		after, err = iptablesOutput(ctx, "-t", "nat", "-S", serviceNAT)
		if err != nil {
			return err
		}
		if strings.Contains(after, "shearwater:backend:") || strings.Contains(after, "10.96.20.1/32") {
			return fmt.Errorf("stale DNAT still intercepts foreign traffic: %s", after)
		}
		addresses, err := s.addrs(foreign.index)
		if err != nil || len(addresses) != 1 {
			return fmt.Errorf("foreign address changed: %v %v", addresses, err)
		}
		if err := k.Teardown(ctx); err != nil {
			return err
		}
		after, err = iptablesOutput(ctx, "-t", "nat", "-S", "OUTPUT")
		if err != nil {
			return err
		}
		if after != before {
			return fmt.Errorf("foreign OUTPUT rule changed: %s -> %s", before, after)
		}
		after, err = iptablesOutput(ctx, "-t", "nat", "-S", "SW-USER-SENTINEL")
		if err != nil {
			return err
		}
		if !strings.Contains(after, "-A SW-USER-SENTINEL -j RETURN") {
			return fmt.Errorf("foreign chain was altered")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
