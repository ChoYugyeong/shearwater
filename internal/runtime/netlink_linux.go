package runtime

// rtnetlink, written out by hand (design decision 6: no CNI plugins, no `ip`
// command). Only what the pod and service networks need: create a bridge and a veth pair,
// set a link up or into a bridge, add an address and a default route, delete a
// link, and look a link up by name.
//
// The message layouts are the kernel's (include/uapi/linux/netlink.h,
// rtnetlink.h, if_link.h, if_addr.h); constants come from golang.org/x/sys/unix.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	goruntime "runtime"
	"time"

	"golang.org/x/sys/unix"
)

var ne = binary.NativeEndian

// vethInfoPeer is VETH_INFO_PEER from linux/veth.h.
const vethInfoPeer = 1

// nsfsMagic is NSFS_MAGIC from linux/magic.h: the filesystem of a bound namespace.
const nsfsMagic = 0x6e736673

type nlSock struct {
	fd  int
	seq uint32
	ctx context.Context
}

// nlOpen opens a route netlink socket in the calling thread's network namespace.
// The socket keeps talking to that namespace whatever the thread does later.
func nlOpen() (*nlSock, error) {
	return nlOpenContext(context.Background())
}

const netlinkTimeout = 5 * time.Second

func nlOpenContext(ctx context.Context) (*nlSock, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("netlink bind: %w", err)
	}
	// A kernel that does not answer must not hang the node agent. Poll
	// sooner for cancellable callers so a server shutdown can interrupt a
	// reconciliation without closing a file descriptor from another thread.
	wait := netlinkTimeout
	if ctx.Done() != nil {
		wait = 250 * time.Millisecond
	}
	timeout := unix.NsecToTimeval(wait.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &timeout); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("netlink receive timeout: %w", err)
	}
	return &nlSock{fd: fd, ctx: ctx}, nil
}

func (s *nlSock) Close() { unix.Close(s.fd) }

type nlMsg struct {
	typ  uint16
	data []byte
}

// request sends one message and collects the answer. A dump ends with
// NLMSG_DONE; anything else is acknowledged with NLMSG_ERROR (code 0 = fine).
func (s *nlSock) request(typ, flags uint16, dump bool, body []byte) ([]nlMsg, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	s.seq++
	flags |= unix.NLM_F_REQUEST
	if dump {
		flags |= unix.NLM_F_DUMP
	} else {
		flags |= unix.NLM_F_ACK
	}
	msg := make([]byte, unix.NLMSG_HDRLEN+len(body))
	ne.PutUint32(msg[0:], uint32(len(msg)))
	ne.PutUint16(msg[4:], typ)
	ne.PutUint16(msg[6:], flags)
	ne.PutUint32(msg[8:], s.seq)
	copy(msg[unix.NLMSG_HDRLEN:], body)
	if err := unix.Sendto(s.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("netlink send: %w", err)
	}
	var out []nlMsg
	buf := make([]byte, 1<<16)
	deadline := time.Now().Add(netlinkTimeout)
	for {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("netlink receive: %w", context.DeadlineExceeded)
		}
		n, _, err := unix.Recvfrom(s.fd, buf, 0)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("netlink receive: %w", err)
		}
		for b := buf[:n]; len(b) >= unix.NLMSG_HDRLEN; {
			l := int(ne.Uint32(b[0:]))
			if l < unix.NLMSG_HDRLEN || l > len(b) {
				return nil, errors.New("netlink: malformed message")
			}
			mtyp, mflags, seq := ne.Uint16(b[4:]), ne.Uint16(b[6:]), ne.Uint32(b[8:])
			payload := b[unix.NLMSG_HDRLEN:l]
			b = b[min(nlAlign(l), len(b)):]
			if seq != s.seq {
				continue
			}
			if mflags&unix.NLM_F_DUMP_INTR != 0 {
				return nil, errors.New("netlink: state changed during dump; retry")
			}
			switch mtyp {
			case unix.NLMSG_DONE:
				if len(payload) >= 4 {
					if code := int32(ne.Uint32(payload)); code != 0 {
						return nil, unix.Errno(-code)
					}
				}
				return out, nil
			case unix.NLMSG_ERROR:
				if len(payload) < 4 {
					return nil, errors.New("netlink: short error message")
				}
				if code := int32(ne.Uint32(payload)); code != 0 {
					return nil, unix.Errno(-code)
				}
				if !dump {
					return out, nil
				}
			default:
				out = append(out, nlMsg{mtyp, bytes.Clone(payload)})
			}
		}
	}
}

func nlAlign(n int) int { return (n + 3) &^ 3 }

func attr(typ uint16, data []byte) []byte {
	b := make([]byte, nlAlign(4+len(data)))
	ne.PutUint16(b[0:], uint16(4+len(data)))
	ne.PutUint16(b[2:], typ)
	copy(b[4:], data)
	return b
}

func attrStr(typ uint16, s string) []byte { return attr(typ, append([]byte(s), 0)) }

func attrU32(typ uint16, v uint32) []byte {
	var b [4]byte
	ne.PutUint32(b[:], v)
	return attr(typ, b[:])
}

func nest(typ uint16, parts ...[]byte) []byte { return attr(typ, bytes.Join(parts, nil)) }

func parseAttrs(b []byte) map[uint16][]byte {
	out := map[uint16][]byte{}
	for len(b) >= 4 {
		l := int(ne.Uint16(b[0:]))
		if l < 4 || l > len(b) {
			break
		}
		out[ne.Uint16(b[2:])&^(unix.NLA_F_NESTED|unix.NLA_F_NET_BYTEORDER)] = b[4:l]
		b = b[min(nlAlign(l), len(b)):]
	}
	return out
}

// ifinfo is struct ifinfomsg: family, pad, type, index, flags, change.
func ifinfo(index int32, flags, change uint32) []byte {
	b := make([]byte, unix.SizeofIfInfomsg)
	ne.PutUint32(b[4:], uint32(index))
	ne.PutUint32(b[8:], flags)
	ne.PutUint32(b[12:], change)
	return b
}

type link struct {
	index  int32
	flags  uint32
	mtu    uint32
	master int32
	peer   int32  // IFLA_LINK: for a veth, the index of the other end
	addr   []byte // hardware address
	kind   string
	alias  string
}

func (l link) up() bool { return l.flags&unix.IFF_UP != 0 }

// linkGet finds a link by name. A missing link is unix.ENODEV.
func (s *nlSock) linkGet(name string) (link, error) {
	msgs, err := s.request(unix.RTM_GETLINK, 0, false, append(ifinfo(0, 0, 0), attrStr(unix.IFLA_IFNAME, name)...))
	if err != nil {
		return link{}, err
	}
	for _, m := range msgs {
		if m.typ != unix.RTM_NEWLINK || len(m.data) < unix.SizeofIfInfomsg {
			continue
		}
		l := link{index: int32(ne.Uint32(m.data[4:])), flags: ne.Uint32(m.data[8:])}
		a := parseAttrs(m.data[unix.SizeofIfInfomsg:])
		if v := a[unix.IFLA_MTU]; len(v) == 4 {
			l.mtu = ne.Uint32(v)
		}
		if v := a[unix.IFLA_MASTER]; len(v) == 4 {
			l.master = int32(ne.Uint32(v))
		}
		if v := a[unix.IFLA_LINK]; len(v) == 4 {
			l.peer = int32(ne.Uint32(v))
		}
		l.addr = a[unix.IFLA_ADDRESS]
		l.kind = string(bytes.TrimRight(parseAttrs(a[unix.IFLA_LINKINFO])[unix.IFLA_INFO_KIND], "\x00"))
		l.alias = string(bytes.TrimRight(a[unix.IFLA_IFALIAS], "\x00"))
		return l, nil
	}
	return link{}, unix.ENODEV
}

func (s *nlSock) linkHasPorts(index int32) (bool, error) {
	msgs, err := s.request(unix.RTM_GETLINK, 0, true, ifinfo(0, 0, 0))
	if err != nil {
		return false, err
	}
	for _, m := range msgs {
		if m.typ == unix.RTM_NEWLINK && len(m.data) >= unix.SizeofIfInfomsg {
			a := parseAttrs(m.data[unix.SizeofIfInfomsg:])
			if v := a[unix.IFLA_MASTER]; len(v) == 4 && int32(ne.Uint32(v)) == index {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *nlSock) linkSetAlias(index int32, alias string) error {
	_, err := s.request(unix.RTM_SETLINK, 0, false, append(ifinfo(index, 0, 0), attrStr(unix.IFLA_IFALIAS, alias)...))
	return err
}

func (s *nlSock) createOwnedLink(name, kind, alias string, extra ...[]byte) (link, error) {
	// Some kernels ignore an alias in RTM_NEWLINK. Mark a temporary link
	// before renaming it so a crash cannot leave the reserved name occupied
	// by a device whose ownership we cannot establish on the next boot.
	var token [4]byte
	rand.Read(token[:])
	temp := "sw-ln-" + hex.EncodeToString(token[:]) // at most 15 visible bytes
	body := bytes.Join(append([][]byte{ifinfo(0, 0, 0), attrStr(unix.IFLA_IFNAME, temp),
		nest(unix.IFLA_LINKINFO, attrStr(unix.IFLA_INFO_KIND, kind))}, extra...), nil)
	if _, err := s.request(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, false, body); err != nil {
		return link{}, fmt.Errorf("create %s device: %w", kind, err)
	}
	l, err := s.linkGet(temp)
	if err != nil {
		return link{}, err
	}
	if err := s.linkSetAlias(l.index, alias); err != nil {
		return link{}, errors.Join(fmt.Errorf("mark %s as owned: %w", temp, err), s.linkDel(l.index))
	}
	if _, err := s.request(unix.RTM_SETLINK, 0, false, append(ifinfo(l.index, 0, 0), attrStr(unix.IFLA_IFNAME, name)...)); err != nil {
		return link{}, errors.Join(fmt.Errorf("rename %s to %s: %w", temp, name, err), s.linkDel(l.index))
	}
	return s.linkGet(name)
}

// linkAddVeth creates a veth pair: name stays in this namespace, the peer is
// created directly inside the namespace behind peerNS, already named peer.
func (s *nlSock) linkAddVeth(name, peer string, mtu uint32, peerNS uintptr) error {
	peerInfo := bytes.Join([][]byte{
		ifinfo(0, 0, 0),
		attrStr(unix.IFLA_IFNAME, peer),
		attrU32(unix.IFLA_MTU, mtu),
		attrU32(unix.IFLA_NET_NS_FD, uint32(peerNS)),
	}, nil)
	body := bytes.Join([][]byte{
		ifinfo(0, 0, 0),
		attrStr(unix.IFLA_IFNAME, name),
		attrU32(unix.IFLA_MTU, mtu),
		nest(unix.IFLA_LINKINFO,
			attr(unix.IFLA_INFO_KIND, []byte("veth")),
			nest(unix.IFLA_INFO_DATA, attr(vethInfoPeer, peerInfo))),
	}, nil)
	_, err := s.request(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, false, body)
	return err
}

func (s *nlSock) linkSetUp(index int32) error {
	_, err := s.request(unix.RTM_SETLINK, 0, false, ifinfo(index, unix.IFF_UP, unix.IFF_UP))
	return err
}

func (s *nlSock) linkSetMaster(index, master int32) error {
	_, err := s.request(unix.RTM_SETLINK, 0, false, append(ifinfo(index, 0, 0), attrU32(unix.IFLA_MASTER, uint32(master))...))
	return err
}

func (s *nlSock) linkSetAddr(index int32, hw []byte) error {
	_, err := s.request(unix.RTM_SETLINK, 0, false, append(ifinfo(index, 0, 0), attr(unix.IFLA_ADDRESS, hw)...))
	return err
}

func (s *nlSock) linkDel(index int32) error {
	_, err := s.request(unix.RTM_DELLINK, 0, false, ifinfo(index, 0, 0))
	return err
}

// addrAdd adds an IPv4 address. Having it already is not an error.
func (s *nlSock) addrAdd(index int32, p netip.Prefix) error {
	_, err := s.request(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL, false, ipv4AddrMessage(index, p))
	if errors.Is(err, unix.EEXIST) {
		return nil
	}
	return err
}

func ipv4AddrMessage(index int32, p netip.Prefix) []byte {
	ip := p.Addr().As4()
	// struct ifaddrmsg: family, prefixlen, flags, scope, index.
	msg := make([]byte, unix.SizeofIfAddrmsg)
	msg[0] = unix.AF_INET
	msg[1] = byte(p.Bits())
	ne.PutUint32(msg[4:], uint32(index))
	return bytes.Join([][]byte{msg, attr(unix.IFA_LOCAL, ip[:]), attr(unix.IFA_ADDRESS, ip[:])}, nil)
}

// forgetIPv4Neighbor removes one learned IP-to-MAC mapping on the host
// bridge. Deleting a veth port can leave this cache entry behind because
// the cache belongs to the bridge, not to the veth that just disappeared.
func (s *nlSock) forgetIPv4Neighbor(index int32, ip netip.Addr) error {
	msg := make([]byte, unix.SizeofNdMsg)
	msg[0] = unix.AF_INET
	ne.PutUint32(msg[4:], uint32(index))
	addr := ip.As4()
	_, err := s.request(unix.RTM_DELNEIGH, 0, false, append(msg, attr(unix.NDA_DST, addr[:])...))
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// addrs lists IPv4 addresses of one link.
func (s *nlSock) addrs(index int32) ([]netip.Prefix, error) {
	all, err := s.ipv4Addresses()
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, a := range all {
		if a.index == index {
			out = append(out, a.prefix)
		}
	}
	return out, nil
}

type linkAddress struct {
	index  int32
	prefix netip.Prefix
}

// ipv4Addresses includes the link index so the caller can distinguish its
// own /32 from an address configured on another interface.
func (s *nlSock) ipv4Addresses() ([]linkAddress, error) {
	msg := make([]byte, unix.SizeofIfAddrmsg)
	msg[0] = unix.AF_INET
	msgs, err := s.request(unix.RTM_GETADDR, 0, true, msg)
	if err != nil {
		return nil, err
	}
	var out []linkAddress
	for _, m := range msgs {
		if m.typ != unix.RTM_NEWADDR || len(m.data) < unix.SizeofIfAddrmsg {
			continue
		}
		a := parseAttrs(m.data[unix.SizeofIfAddrmsg:])
		v := a[unix.IFA_LOCAL]
		if len(v) != 4 {
			v = a[unix.IFA_ADDRESS]
		}
		if len(v) == 4 {
			out = append(out, linkAddress{int32(ne.Uint32(m.data[4:])), netip.PrefixFrom(netip.AddrFrom4([4]byte(v)), int(m.data[1]))})
		}
	}
	return out, nil
}

type ipv4Route struct {
	prefix netip.Prefix
	index  int32 // the output interface, or zero for routes without RTA_OIF
	kind   uint8
}

// ipv4Routes reads every routing table in this namespace. Looking up an
// address with RTM_GETROUTE alone would only return the winning route. Once
// our own /32 exists, it would hide a conflicting route added by a VPN.
func (s *nlSock) ipv4Routes() ([]ipv4Route, error) {
	msg := make([]byte, unix.SizeofRtMsg)
	msg[0] = unix.AF_INET
	msgs, err := s.request(unix.RTM_GETROUTE, 0, true, msg)
	if err != nil {
		return nil, err
	}
	var out []ipv4Route
	for _, m := range msgs {
		if m.typ != unix.RTM_NEWROUTE || len(m.data) < unix.SizeofRtMsg || m.data[0] != unix.AF_INET {
			continue
		}
		bits := int(m.data[1])
		if bits == 0 {
			continue // the normal default route is expected on every host
		}
		a := parseAttrs(m.data[unix.SizeofRtMsg:])
		dst := a[unix.RTA_DST]
		if len(dst) != 4 || bits > 32 {
			return nil, errors.New("netlink: malformed IPv4 route destination")
		}
		r := ipv4Route{prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte(dst)), bits).Masked(), kind: m.data[7]}
		if oif := a[unix.RTA_OIF]; len(oif) == 4 {
			r.index = int32(ne.Uint32(oif))
		}
		out = append(out, r)
	}
	return out, nil
}

// routeAddDefault adds "default via gw dev index". Having it already is not an error.
func (s *nlSock) routeAddDefault(gw netip.Addr, index int32) error {
	g := gw.As4()
	// struct rtmsg: family, dst_len, src_len, tos, table, protocol, scope, type, flags.
	msg := make([]byte, unix.SizeofRtMsg)
	msg[0] = unix.AF_INET
	msg[4] = unix.RT_TABLE_MAIN
	msg[5] = unix.RTPROT_BOOT
	msg[6] = unix.RT_SCOPE_UNIVERSE
	msg[7] = unix.RTN_UNICAST
	body := bytes.Join([][]byte{msg, attr(unix.RTA_GATEWAY, g[:]), attrU32(unix.RTA_OIF, uint32(index))}, nil)
	_, err := s.request(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_EXCL, false, body)
	if errors.Is(err, unix.EEXIST) {
		return nil
	}
	return err
}

// ---- network namespaces ----

// isNetns reports whether path is a bound network namespace (and not a plain
// file left behind by an interrupted setup, or by a reboot that kept the file).
func isNetns(path string) bool {
	var st unix.Statfs_t
	return unix.Statfs(path, &st) == nil && st.Type == nsfsMagic
}

// onLockedThread runs fn on a thread of its own and lets that thread die
// afterwards: a goroutine that exits while locked takes its thread with it. So
// whatever fn does to the thread's namespaces never reaches another goroutine.
func onLockedThread(fn func() error) error {
	errc := make(chan error, 1)
	go func() {
		goruntime.LockOSThread()
		errc <- fn()
	}()
	return <-errc
}

// nsCreate makes a new network namespace and binds it at path, which keeps it
// alive with no process in it. Unbinding it (nsDelete) is our job.
func nsCreate(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o444)
	if err != nil {
		return err
	}
	f.Close()
	err = onLockedThread(func() error {
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			return fmt.Errorf("unshare: %w", err)
		}
		src := fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid())
		if err := unix.Mount(src, path, "none", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind %s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("create network namespace: %w", err)
	}
	return nil
}

// nsDelete unbinds the namespace and removes the file. Gone already is fine.
func nsDelete(path string) error {
	if err := unix.Unmount(path, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("unmount %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// inNetns runs fn with a netlink socket that lives in the namespace at path.
func inNetns(path string, fn func(s *nlSock) error) error {
	return onLockedThread(func() error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := unix.Setns(int(f.Fd()), unix.CLONE_NEWNET); err != nil {
			return fmt.Errorf("enter %s: %w", path, err)
		}
		s, err := nlOpen()
		if err != nil {
			return err
		}
		defer s.Close()
		return fn(s)
	})
}
