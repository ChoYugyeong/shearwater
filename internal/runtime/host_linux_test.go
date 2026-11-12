package runtime

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestUnmarkedVethNeedsItsPodPeer(t *testing.T) {
	const uid = "bfc6eff0-bc78-4e21-b292-563a1d94650d"
	host := link{kind: "veth", index: 12, peer: 3, master: 7}
	br := link{index: 7}
	if ownedVeth(host, br, link{}, uid) {
		t.Fatal("same bridge and interface name cannot establish ownership")
	}
	peer := link{kind: "veth", index: 3, peer: 12}
	if !ownedVeth(host, br, peer, uid) {
		t.Fatal("a veth with the actual Pod peer could not be recovered")
	}
}

// The extra chain is created only in a new network namespace. Test both
// setup and teardown so neither erases a site's rules when names collide.
func TestForeignFirewallChainIsPreserved(t *testing.T) {
	if os.Getenv("SW_TEST_SERVICE_NETNS") != "1" {
		t.Skip("set SW_TEST_SERVICE_NETNS=1 in an isolated Linux test run with root")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	if err := onLockedThread(func() error {
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			return err
		}
		n := NewNet("", netip.MustParsePrefix("10.244.0.0/24"), netip.MustParsePrefix("10.96.0.0/16"))
		if _, err := iptablesOutput(ctx, "-t", "filter", "-N", forwardChain); err != nil {
			return err
		}
		if _, err := iptablesOutput(ctx, "-t", "filter", "-A", forwardChain, "-j", "DROP"); err != nil {
			return err
		}
		before, err := iptablesOutput(ctx, "-t", "filter", "-S", forwardChain)
		if err != nil {
			return err
		}
		if n.ensureRules(ctx) == nil || n.removeRules(ctx) == nil {
			return fmt.Errorf("setup or teardown accepted a foreign rule")
		}
		after, err := iptablesOutput(ctx, "-t", "filter", "-S", forwardChain)
		if err != nil || after != before {
			return fmt.Errorf("foreign chain changed: %q to %q (%v)", before, after, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
