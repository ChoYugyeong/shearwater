package runtime

// Conntrack is the kernel's connection table. DNAT is chosen for the first
// packet of a TCP connection; later packets retain that choice even when sw
// stops or replaces its rules. These small netlink messages use Linux UAPI
// nfnetlink_conntrack.h and nf_conntrack_tcp.h; no conntrack CLI is required.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"golang.org/x/sys/unix"
)

type serviceConnection struct {
	service  netip.Addr
	pod      netip.Addr
	active   bool
	identity []byte // exact original tuple + ID (+ zone), never a whole-table delete
}

func checkedAttrs(b []byte) (map[uint16][]byte, error) {
	out := make(map[uint16][]byte)
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, errors.New("conntrack: truncated attribute")
		}
		n := int(ne.Uint16(b))
		if n < 4 || n > len(b) || nlAlign(n) > len(b) {
			return nil, errors.New("conntrack: invalid attribute length")
		}
		key := ne.Uint16(b[2:]) & 0x3fff
		if _, duplicate := out[key]; duplicate {
			return nil, errors.New("conntrack: duplicate attribute")
		}
		out[key] = b[4:n]
		b = b[nlAlign(n):]
	}
	return out, nil
}

func parseServiceConnection(b []byte, pods, services netip.Prefix) (serviceConnection, bool, error) {
	var c serviceConnection
	if len(b) < 4 {
		return c, false, errors.New("conntrack: short nfgenmsg")
	}
	if b[0] != unix.AF_INET {
		return c, false, nil
	}
	a, err := checkedAttrs(b[4:])
	if err != nil {
		return c, false, err
	}
	orig, err := checkedAttrs(a[1])
	if err != nil {
		return c, false, err
	}
	ip, err := checkedAttrs(orig[1])
	if err != nil {
		return c, false, err
	}
	proto, err := checkedAttrs(orig[2])
	if err != nil {
		return c, false, err
	}
	if len(proto[1]) != 1 {
		return c, false, errors.New("conntrack: missing protocol")
	}
	if proto[1][0] != unix.IPPROTO_TCP {
		return c, false, nil
	}
	if len(ip[2]) != 4 {
		return c, false, errors.New("conntrack: missing destination")
	}
	c.service = netip.AddrFrom4([4]byte(ip[2]))
	if !services.Contains(c.service) {
		return c, false, nil
	}
	reply, err := checkedAttrs(a[2])
	if err != nil {
		return c, false, err
	}
	rip, err := checkedAttrs(reply[1])
	if err != nil {
		return c, false, err
	}
	if len(rip[1]) != 4 {
		return c, false, errors.New("conntrack: missing reply source")
	}
	c.pod = netip.AddrFrom4([4]byte(rip[1]))
	if !pods.Contains(c.pod) {
		return c, false, nil
	}
	info, err := checkedAttrs(a[4])
	if err != nil {
		return c, false, err
	}
	tcp, err := checkedAttrs(info[1])
	if err != nil {
		return c, false, err
	}
	if len(tcp[1]) != 1 || len(a[12]) != 4 {
		return c, false, errors.New("conntrack: missing TCP state or ID")
	}
	// TIME_WAIT (7) and CLOSE (8) no longer carry application data. All
	// other states, including unknown future states, conservatively drain.
	c.active = tcp[1][0] != 7 && tcp[1][0] != 8
	c.identity = append(attr(1|unix.NLA_F_NESTED, a[1]), attr(12, a[12])...)
	if zone, ok := a[18]; ok {
		if len(zone) != 2 {
			return c, false, errors.New("conntrack: invalid zone")
		}
		c.identity = append(c.identity, attr(18, zone)...)
	}
	return c, true, nil
}

func (k *KernelServices) connections(ctx context.Context) ([]serviceConnection, error) {
	s, err := nlOpenProtocol(ctx, unix.NETLINK_NETFILTER)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	msgs, err := s.request(1<<8|1, 0, true, []byte{unix.AF_INET, 0, 0, 0})
	if err != nil {
		return nil, fmt.Errorf("read Service connections: %w", err)
	}
	var result []serviceConnection
	for _, m := range msgs {
		c, owned, err := parseServiceConnection(m.data, k.PodCIDR, k.ServiceCIDR)
		if err != nil {
			return nil, err
		}
		if owned {
			result = append(result, c)
		}
	}
	return result, nil
}

func (k *KernelServices) Counts(ctx context.Context) (map[netip.Addr]int, error) {
	connections, err := k.connections(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[netip.Addr]int)
	for _, c := range connections {
		if c.active {
			out[c.pod]++
		}
	}
	return out, nil
}

// forget removes only matching Service-to-Pod NAT tuples. The kernel checks
// CTA_ID as well as the tuple, so a reused client port cannot delete a newer
// connection that arrived between the dump and this request.
func (k *KernelServices) forget(ctx context.Context, match func(serviceConnection) bool) error {
	connections, err := k.connections(ctx)
	if err != nil {
		return err
	}
	s, err := nlOpenProtocol(ctx, unix.NETLINK_NETFILTER)
	if err != nil {
		return err
	}
	defer s.Close()
	for _, c := range connections {
		if !match(c) {
			continue
		}
		_, err := s.request(1<<8|2, 0, false, append([]byte{unix.AF_INET, 0, 0, 0}, c.identity...))
		if err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("retire Service connection: %w", err)
		}
	}
	return nil
}

func (k *KernelServices) ForgetPod(ctx context.Context, ip netip.Addr) error {
	if !k.PodCIDR.Contains(ip) {
		return errors.New("refusing conntrack cleanup outside Pod CIDR")
	}
	return k.forget(ctx, func(c serviceConnection) bool { return c.pod == ip })
}
