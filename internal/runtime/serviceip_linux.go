package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"golang.org/x/sys/unix"
)

const (
	serviceLink      = "sw-svc0"
	serviceLinkAlias = "shearwater:clusterip"
)

// ServiceAddresses gives the kernel a local route for each ClusterIP. A dummy
// interface owns the address; kernel DNAT selects the actual Pod destination.
// We claim individual /32s so addresses outside active Services stay untouched.
type ServiceAddresses struct {
	CIDR netip.Prefix
	// Kernel forwarding installs addresses before rules, but must remove
	// obsolete DNAT BEFORE detaching addresses. A first pass can defer only
	// the deletions; the final pass completes them after the rules commit.
	retainStale bool
}

// Reconcile validates and attaches desired addresses. The kernel data path
// uses retainStale for its first pass, then detaches obsolete addresses only
// after withdrawing their forwarding rules. A restart reuses owned addresses.
func (a ServiceAddresses) Reconcile(ctx context.Context, wanted map[netip.Addr]bool) (map[netip.Addr]bool, error) {
	for ip := range wanted {
		if !ip.Is4() || !a.CIDR.Contains(ip) {
			return nil, fmt.Errorf("ClusterIP %s is outside %s", ip, a.CIDR)
		}
	}
	s, err := nlOpenContext(ctx)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	l, err := s.linkGet(serviceLink)
	if errors.Is(err, unix.ENODEV) {
		if len(wanted) == 0 {
			return nil, nil
		}
		l, err = s.createOwnedLink(serviceLink, "dummy", serviceLinkAlias)
	}
	if err != nil {
		return nil, err
	}
	// A reserved name alone does not identify our link. Refuse to touch a
	// foreign link, even when it happens to have the expected name and kind.
	if l.kind != "dummy" || l.alias != serviceLinkAlias {
		if len(wanted) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("interface %s already exists without the Shearwater dummy marker", serviceLink)
	}
	if len(wanted) > 0 && !l.up() {
		if err := s.linkSetUp(l.index); err != nil {
			return nil, fmt.Errorf("bring %s up: %w", serviceLink, err)
		}
	}
	locals, err := s.ipv4Addresses()
	if err != nil {
		return nil, err
	}
	owned, foreign := make(map[netip.Addr]bool), make(map[netip.Addr]bool)
	for _, local := range locals {
		ip := local.prefix.Addr()
		if !a.CIDR.Contains(ip) {
			continue
		}
		if local.index == l.index && local.prefix.Bits() == 32 {
			owned[ip] = true
		} else {
			foreign[ip] = true
		}
	}
	var routes []ipv4Route
	if len(wanted) > 0 {
		routes, err = s.ipv4Routes()
		if err != nil {
			return nil, err
		}
	}
	attached, allowed := make(map[netip.Addr]bool), make(map[netip.Addr]bool)
	var failures []error
	for ip := range wanted {
		if foreign[ip] {
			failures = append(failures, fmt.Errorf("ClusterIP %s already exists as a host address", ip))
		} else if p := conflictingRoute(ip, l.index, owned[ip], routes); p.IsValid() {
			failures = append(failures, fmt.Errorf("ClusterIP %s overlaps host route %s", ip, p))
		} else {
			allowed[ip] = true
		}
	}
	// If an external route appeared since the last scan, detach our own /32
	// to give the kernel that route back. Leave all other links and routes
	// untouched. A detach failure must not take down unrelated Services.
	for ip := range owned {
		if allowed[ip] {
			attached[ip] = true
			continue
		}
		if a.retainStale {
			continue
		}
		_, err := s.request(unix.RTM_DELADDR, 0, false, ipv4AddrMessage(l.index, netip.PrefixFrom(ip, 32)))
		if err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) {
			failures = append(failures, fmt.Errorf("detach ClusterIP %s from %s: %w", ip, serviceLink, err))
		}
	}
	var add []netip.Addr
	for ip := range allowed {
		if !attached[ip] {
			add = append(add, ip)
		}
	}
	slices.SortFunc(add, netip.Addr.Compare)
	for _, ip := range add {
		if err := ctx.Err(); err != nil {
			return attached, err
		}
		if err := s.addrAdd(l.index, netip.PrefixFrom(ip, 32)); err != nil {
			failures = append(failures, fmt.Errorf("attach ClusterIP %s to %s: %w", ip, serviceLink, err))
		} else {
			attached[ip] = true
		}
	}
	return attached, errors.Join(failures...)
}

// An interface address installs a local route that wins over a more general
// route. Check all non-default routes so our /32 cannot steal an address from
// a connected subnet, VPN, or a deliberately unreachable prefix.
func conflictingRoute(ip netip.Addr, index int32, owned bool, routes []ipv4Route) netip.Prefix {
	for _, r := range routes {
		if r.prefix.Bits() == 0 || !r.prefix.Contains(ip) {
			continue
		}
		if owned && r.prefix.Bits() == 32 && r.index == index && r.kind == unix.RTN_LOCAL {
			continue // the kernel installed this route for our existing address
		}
		return r.prefix
	}
	return netip.Prefix{}
}
