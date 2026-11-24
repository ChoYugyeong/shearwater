package runtime

// The pod network between nodes (docs/design-multinode-tenancy.md 3.3): one
// VXLAN device per node, the way flannel's vxlan backend works. Each node
// publishes the MAC address of its device; every other node then routes that
// node's /24 through the device with a permanent ARP entry for the node's
// network address and a forwarding entry that sends that MAC to the node's
// underlay address. Nothing is learned from the wire, so a node only reaches
// the nodes the control plane told it about.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"golang.org/x/sys/unix"
)

const (
	OverlayName  = "sw-vxlan0"
	overlayAlias = "shearwater:overlay"
	// OverlayPort is flannel's VXLAN port; the IANA port 4789 is often
	// taken by Docker's overlay network.
	OverlayPort = 8472
	overlayVNI  = 1
	// OverlayOverhead is what VXLAN adds to each packet (IPv4, UDP, VXLAN
	// and inner Ethernet headers).
	OverlayOverhead = 50
	// routeProto marks our routes so they can be found again in a dump.
	routeProto = 0x5a
)

// IFLA_VXLAN_* from linux/if_link.h.
const (
	iflaVXLANID       = 1
	iflaVXLANLocal    = 4
	iflaVXLANLearning = 7
	iflaVXLANPort     = 15
)

// Peer is another node's end of the overlay.
type Peer struct {
	PodCIDR netip.Prefix     // that node's /24
	NodeIP  netip.Addr       // where its VXLAN packets go
	MAC     net.HardwareAddr // its VXLAN device's address
}

// Overlay is this node's VXLAN device.
type Overlay struct {
	LocalIP netip.Addr   // this node's underlay address
	PodCIDR netip.Prefix // this node's /24; its network address goes on the device
}

func be16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }

// Ensure creates or checks the device and returns its MAC address.
func (o *Overlay) Ensure() (net.HardwareAddr, error) {
	if !o.LocalIP.Is4() || !o.PodCIDR.Addr().Is4() {
		return nil, fmt.Errorf("overlay needs an IPv4 node address and pod range, got %s and %s", o.LocalIP, o.PodCIDR)
	}
	s, err := nlOpen()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	l, err := s.linkGet(OverlayName)
	switch {
	case err == nil && l.alias != overlayAlias:
		return nil, fmt.Errorf("a device named %s exists and was not made by Shearwater", OverlayName)
	case err == nil:
		if cur, ok := s.vxlanLocal(OverlayName); !ok || cur != o.LocalIP {
			// The node address changed: rebuild the device.
			if err := s.linkDel(l.index); err != nil {
				return nil, err
			}
			err = unix.ENODEV
		}
	}
	if errors.Is(err, unix.ENODEV) {
		local := o.LocalIP.As4()
		mtu := uplinkMTU() - OverlayOverhead
		info := nest(unix.IFLA_LINKINFO, attrStr(unix.IFLA_INFO_KIND, "vxlan"), nest(unix.IFLA_INFO_DATA,
			attrU32(iflaVXLANID, overlayVNI), attr(iflaVXLANLocal, local[:]),
			attr(iflaVXLANLearning, []byte{0}), attr(iflaVXLANPort, be16(OverlayPort))))
		body := bytes.Join([][]byte{ifinfo(0, 0, 0), attrStr(unix.IFLA_IFNAME, OverlayName), attrU32(unix.IFLA_MTU, mtu), info}, nil)
		if _, err := s.request(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, false, body); err != nil {
			return nil, fmt.Errorf("create %s (is the vxlan kernel module available?): %w", OverlayName, err)
		}
		if l, err = s.linkGet(OverlayName); err != nil {
			return nil, err
		}
		if err := s.linkSetAlias(l.index, overlayAlias); err != nil {
			return nil, errors.Join(err, s.linkDel(l.index))
		}
	} else if err != nil {
		return nil, err
	}
	if err := s.addrAdd(l.index, netip.PrefixFrom(o.PodCIDR.Masked().Addr(), 32)); err != nil {
		return nil, fmt.Errorf("address of %s: %w", OverlayName, err)
	}
	if !l.up() {
		if err := s.linkSetUp(l.index); err != nil {
			return nil, err
		}
	}
	return net.HardwareAddr(l.addr), nil
}

// vxlanLocal reads the device's local (underlay) address.
func (s *nlSock) vxlanLocal(name string) (netip.Addr, bool) {
	msgs, err := s.request(unix.RTM_GETLINK, 0, false, append(ifinfo(0, 0, 0), attrStr(unix.IFLA_IFNAME, name)...))
	if err != nil {
		return netip.Addr{}, false
	}
	for _, m := range msgs {
		if m.typ != unix.RTM_NEWLINK || len(m.data) < unix.SizeofIfInfomsg {
			continue
		}
		info := parseAttrs(parseAttrs(m.data[unix.SizeofIfInfomsg:])[unix.IFLA_LINKINFO])
		local := parseAttrs(info[unix.IFLA_INFO_DATA])[iflaVXLANLocal]
		if len(local) == 4 {
			return netip.AddrFrom4([4]byte(local)), true
		}
	}
	return netip.Addr{}, false
}

func neighMsg(family uint8, index int32, flags uint8) []byte {
	// struct ndmsg: family, pad1, pad2, ifindex, state, flags, type.
	msg := make([]byte, unix.SizeofNdMsg)
	msg[0] = family
	ne.PutUint32(msg[4:], uint32(index))
	ne.PutUint16(msg[8:], unix.NUD_PERMANENT)
	msg[10] = flags
	return msg
}

func routeMsg(dst netip.Prefix) []byte {
	msg := make([]byte, unix.SizeofRtMsg)
	msg[0] = unix.AF_INET
	msg[1] = byte(dst.Bits())
	msg[4] = unix.RT_TABLE_MAIN
	msg[5] = routeProto
	msg[6] = unix.RT_SCOPE_UNIVERSE
	msg[7] = unix.RTN_UNICAST
	ne.PutUint32(msg[8:], unix.RTNH_F_ONLINK)
	return msg
}

// SetPeers makes the device reach exactly peers: routes, ARP and forwarding
// entries for new or changed peers are written, those of peers that left are
// removed.
func (o *Overlay) SetPeers(peers []Peer) error {
	s, err := nlOpen()
	if err != nil {
		return err
	}
	defer s.Close()
	l, err := s.linkGet(OverlayName)
	if err != nil {
		return fmt.Errorf("%s: %w", OverlayName, err)
	}
	want := map[netip.Prefix]Peer{}
	for _, p := range peers {
		if p.PodCIDR.Masked() == o.PodCIDR.Masked() || !p.NodeIP.Is4() || len(p.MAC) != 6 {
			continue
		}
		want[p.PodCIDR.Masked()] = p
	}
	for cidr, p := range want {
		gw := cidr.Addr().As4()
		ip := p.NodeIP.As4()
		// ARP: the peer's network address has the peer's device MAC.
		if _, err := s.request(unix.RTM_NEWNEIGH, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, false,
			bytes.Join([][]byte{neighMsg(unix.AF_INET, l.index, 0), attr(unix.NDA_DST, gw[:]), attr(unix.NDA_LLADDR, p.MAC)}, nil)); err != nil {
			return fmt.Errorf("ARP entry for %s: %w", cidr, err)
		}
		// Forwarding: that MAC lives behind the peer's underlay address.
		if _, err := s.request(unix.RTM_NEWNEIGH, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, false,
			bytes.Join([][]byte{neighMsg(unix.AF_BRIDGE, l.index, unix.NTF_SELF), attr(unix.NDA_LLADDR, p.MAC), attr(unix.NDA_DST, ip[:])}, nil)); err != nil {
			return fmt.Errorf("forwarding entry for %s: %w", cidr, err)
		}
		dst := cidr.Addr().As4()
		if _, err := s.request(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, false,
			bytes.Join([][]byte{routeMsg(cidr), attr(unix.RTA_DST, dst[:]), attr(unix.RTA_GATEWAY, gw[:]), attrU32(unix.RTA_OIF, uint32(l.index))}, nil)); err != nil {
			return fmt.Errorf("route to %s: %w", cidr, err)
		}
	}
	// Remove what belongs to peers that are gone.
	routes, err := s.overlayRoutes(l.index)
	if err != nil {
		return err
	}
	for _, cidr := range routes {
		if _, ok := want[cidr]; ok {
			continue
		}
		dst := cidr.Addr().As4()
		if _, err := s.request(unix.RTM_DELROUTE, 0, false, bytes.Join([][]byte{routeMsg(cidr), attr(unix.RTA_DST, dst[:]), attrU32(unix.RTA_OIF, uint32(l.index))}, nil)); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("remove route to %s: %w", cidr, err)
		}
	}
	return s.pruneNeighbors(l.index, want)
}

// overlayRoutes lists our routes through the device.
func (s *nlSock) overlayRoutes(index int32) ([]netip.Prefix, error) {
	msg := make([]byte, unix.SizeofRtMsg)
	msg[0] = unix.AF_INET
	msgs, err := s.request(unix.RTM_GETROUTE, 0, true, msg)
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, m := range msgs {
		if m.typ != unix.RTM_NEWROUTE || len(m.data) < unix.SizeofRtMsg || m.data[5] != routeProto {
			continue
		}
		a := parseAttrs(m.data[unix.SizeofRtMsg:])
		oif, dst := a[unix.RTA_OIF], a[unix.RTA_DST]
		if len(oif) == 4 && int32(ne.Uint32(oif)) == index && len(dst) == 4 {
			out = append(out, netip.PrefixFrom(netip.AddrFrom4([4]byte(dst)), int(m.data[1])))
		}
	}
	return out, nil
}

// pruneNeighbors removes ARP and forwarding entries on the device that no
// wanted peer explains.
func (s *nlSock) pruneNeighbors(index int32, want map[netip.Prefix]Peer) error {
	var gws []netip.Addr
	var macs [][]byte
	for cidr, p := range want {
		gws = append(gws, cidr.Addr())
		macs = append(macs, p.MAC)
	}
	for _, family := range []uint8{unix.AF_INET, unix.AF_BRIDGE} {
		msgs, err := s.request(unix.RTM_GETNEIGH, 0, true, neighMsg(family, 0, 0))
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.typ != unix.RTM_NEWNEIGH || len(m.data) < unix.SizeofNdMsg || int32(ne.Uint32(m.data[4:])) != index {
				continue
			}
			a := parseAttrs(m.data[unix.SizeofNdMsg:])
			var del []byte
			switch family {
			case unix.AF_INET:
				dst := a[unix.NDA_DST]
				if len(dst) != 4 || slices.Contains(gws, netip.AddrFrom4([4]byte(dst))) {
					continue
				}
				del = bytes.Join([][]byte{neighMsg(family, index, 0), attr(unix.NDA_DST, dst)}, nil)
			case unix.AF_BRIDGE:
				mac, dst := a[unix.NDA_LLADDR], a[unix.NDA_DST]
				if len(mac) != 6 || len(dst) != 4 || slices.ContainsFunc(macs, func(w []byte) bool { return bytes.Equal(w, mac) }) {
					continue
				}
				del = bytes.Join([][]byte{neighMsg(family, index, unix.NTF_SELF), attr(unix.NDA_LLADDR, mac), attr(unix.NDA_DST, dst)}, nil)
			}
			if _, err := s.request(unix.RTM_DELNEIGH, 0, false, del); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
	}
	return nil
}

// TeardownOverlay removes our device (sw down --all).
func TeardownOverlay() error {
	s, err := nlOpen()
	if err != nil {
		return err
	}
	defer s.Close()
	l, err := s.linkGet(OverlayName)
	if errors.Is(err, unix.ENODEV) {
		return nil
	}
	if err != nil {
		return err
	}
	if l.alias != overlayAlias {
		return nil
	}
	return s.linkDel(l.index)
}

// overlayIndex returns our device's index, or 0.
func (s *nlSock) overlayIndex() int32 {
	l, err := s.linkGet(OverlayName)
	if err != nil || l.alias != overlayAlias {
		return 0
	}
	return l.index
}
