package runtime

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// Run only on explicit opt-in with CAP_SYS_ADMIN. Both dummy interfaces and
// the collision routes live in a private namespace on a single OS thread.
// If unshare fails we stop before changing any interface or address.
func TestServiceAddressesProtectHostRoutes(t *testing.T) {
	if os.Getenv("SW_TEST_SERVICE_NETNS") != "1" {
		t.Skip("set SW_TEST_SERVICE_NETNS=1 in an isolated Linux test run with root")
	}
	ctx := t.Context()
	if err := onLockedThread(func() error {
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			return fmt.Errorf("isolate test network namespace: %w", err)
		}
		return checkServiceRoutes(ctx)
	}); err != nil {
		t.Fatal(err)
	}
}

func checkServiceRoutes(ctx context.Context) error {
	s, err := nlOpenContext(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	const other = "sw-test-remote"
	body := bytes.Join([][]byte{ifinfo(0, 0, 0), attrStr(unix.IFLA_IFNAME, other),
		nest(unix.IFLA_LINKINFO, attrStr(unix.IFLA_INFO_KIND, "dummy"))}, nil)
	if _, err := s.request(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, false, body); err != nil {
		return err
	}
	l, err := s.linkGet(other)
	if err != nil {
		return err
	}
	if err := s.linkSetUp(l.index); err != nil {
		return err
	}
	// Connected /24s cover the Service IP without having the same address.
	if err := s.addrAdd(l.index, netip.MustParsePrefix("10.96.1.254/24")); err != nil {
		return err
	}
	a := ServiceAddresses{CIDR: netip.MustParsePrefix("10.96.0.0/16")}
	first, later, healthy := netip.MustParseAddr("10.96.1.1"), netip.MustParseAddr("10.96.2.1"), netip.MustParseAddr("10.96.3.1")
	wanted := map[netip.Addr]bool{first: true, later: true, healthy: true}
	have, err := a.Reconcile(ctx, wanted)
	if err == nil || !maps.Equal(have, map[netip.Addr]bool{later: true, healthy: true}) {
		return fmt.Errorf("initial collision: attached %v, error %v", have, err)
	}
	if err := s.addrAdd(l.index, netip.MustParsePrefix("10.96.2.254/24")); err != nil {
		return err
	}
	have, err = a.Reconcile(ctx, wanted)
	if err == nil || !maps.Equal(have, map[netip.Addr]bool{healthy: true}) {
		return fmt.Errorf("collision after attachment: attached %v, error %v", have, err)
	}
	service, err := s.linkGet(serviceLink)
	if err != nil {
		return err
	}
	ips, err := s.addrs(service.index)
	if err != nil || service.alias != serviceLinkAlias || len(ips) != 1 || ips[0] != netip.PrefixFrom(healthy, 32) {
		return fmt.Errorf("managed link after collision: alias %q, addresses %v, error %v", service.alias, ips, err)
	}
	otherIPs, err := s.addrs(l.index)
	if err != nil || len(otherIPs) != 2 {
		return fmt.Errorf("foreign addresses changed: %v, error %v", otherIPs, err)
	}
	return nil
}
