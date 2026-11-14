package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Names of what the pod network owns on the host (design 7).
const (
	BridgeName      = "sw-br0"
	forwardChain    = "SHEARWATER-FORWARD"
	postroutingName = "SHEARWATER-POSTROUTING"
)

// Net is the pod network of design 7, decision 6: a bridge, one veth pair per
// pod, and two iptables chains. Everything is done over netlink except the
// rules, which go through the iptables binary.
type Net struct {
	Bridge      string
	Dir         string // where pod namespaces are bound, /run/shearwater/netns
	PodCIDR     netip.Prefix
	ServiceCIDR netip.Prefix

	mu  sync.Mutex
	mtu uint32
}

func NewNet(runDir string, podCIDR, serviceCIDR netip.Prefix) *Net {
	return &Net{Bridge: BridgeName, Dir: filepath.Join(runDir, "netns"), PodCIDR: podCIDR.Masked(), ServiceCIDR: serviceCIDR.Masked()}
}

// Gateway is the bridge's address: the first one of the pod range.
func (n *Net) Gateway() netip.Addr { return n.PodCIDR.Addr().Next() }

func (n *Net) Path(podUID string) string { return filepath.Join(n.Dir, podUID) }

// hostVeth is the host end of a pod's veth pair: "sw" and the first 8
// characters of the uid (interface names hold 15).
func hostVeth(podUID string) string { return "sw" + podUID[:8] }

func vethAlias(podUID string) string { return "shearwater:pod:" + podUID }

func ownedVeth(l, bridge, podPeer link, uid string) bool {
	if l.kind != "veth" || (l.master != 0 && l.master != bridge.index) {
		return false
	}
	// A legacy or partially created link has no alias. The two veth ends
	// must point to one another, with the peer in this Pod's namespace.
	return l.alias == vethAlias(uid) ||
		(l.alias == "" && podPeer.kind == "veth" && l.peer == podPeer.index && podPeer.peer == l.index)
}

// uplinkMTU is the MTU of the interface the default route leaves through. Pods
// get the same one: a larger one stalls on big answers (design 7).
func uplinkMTU() uint32 {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return 1500
	}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		if b, err := os.ReadFile("/sys/class/net/" + f[0] + "/mtu"); err == nil {
			if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32); err == nil && v >= 576 {
				return uint32(v)
			}
		}
	}
	return 1500
}

func (n *Net) EnsureHost() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := CheckIPv4Forwarding(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := n.CheckHostRanges(ctx); err != nil {
		return err
	}
	if err := os.MkdirAll(n.Dir, 0o755); err != nil {
		return err
	}
	n.mtu = uplinkMTU()

	s, err := nlOpen()
	if err != nil {
		return err
	}
	defer s.Close()
	br, err := n.checkedBridge(s)
	if errors.Is(err, unix.ENODEV) {
		br, err = s.createOwnedLink(n.Bridge, "bridge", bridgeAlias, attrU32(unix.IFLA_MTU, n.mtu))
	}
	if err != nil {
		return fmt.Errorf("bridge %s: %w", n.Bridge, err)
	}
	// A bridge takes the lowest address among its ports, so it would change as
	// pods come and go and their neighbour entries for the gateway would go
	// stale. An address set by hand stays. It has to differ from the one the
	// bridge was born with: the kernel only registers the address as local
	// (and so delivers frames for it) when a set really changes it. Setting it
	// to its own value pins it but leaves the bridge deaf (seen on this host).
	// 0a:58 followed by the gateway's IPv4 address is the same every time.
	if br.alias == "" {
		if err := s.linkSetAlias(br.index, bridgeAlias); err != nil {
			return fmt.Errorf("mark bridge %s: %w", n.Bridge, err)
		}
	}
	if mac := n.bridgeMAC(); !bytes.Equal(br.addr, mac) {
		if err := s.linkSetAddr(br.index, mac); err != nil {
			return fmt.Errorf("set the address of %s: %w", n.Bridge, err)
		}
	}
	if err := s.addrAdd(br.index, netip.PrefixFrom(n.Gateway(), n.PodCIDR.Bits())); err != nil {
		return fmt.Errorf("address of %s: %w", n.Bridge, err)
	}
	if !br.up() {
		if err := s.linkSetUp(br.index); err != nil {
			return fmt.Errorf("bring up %s: %w", n.Bridge, err)
		}
	}
	return n.ensureRules(ctx)
}

func (n *Net) Setup(podUID string, ip netip.Addr) (created bool, err error) {
	if !IsUID(podUID) {
		return false, fmt.Errorf("network setup: %q is not a pod uid", podUID)
	}
	if !n.PodCIDR.Contains(ip) || ip == n.Gateway() {
		return false, fmt.Errorf("network setup: %s is not a pod address in %s", ip, n.PodCIDR)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	path, host := n.Path(podUID), hostVeth(podUID)
	if !isNetns(path) {
		// A plain file here is what an interrupted setup leaves.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err := nsCreate(path); err != nil {
			return false, err
		}
		created = true
	}

	hs, err := nlOpen()
	if err != nil {
		return created, err
	}
	defer hs.Close()
	br, err := n.checkedBridge(hs)
	if err != nil {
		return created, fmt.Errorf("bridge %s: %w", n.Bridge, err)
	}
	var eth0 link
	if err := inNetns(path, func(s *nlSock) error {
		var err error
		eth0, err = s.linkGet("eth0")
		if errors.Is(err, unix.ENODEV) {
			return nil
		}
		return err
	}); err != nil {
		return created, fmt.Errorf("look into %s: %w", path, err)
	}
	newPair := eth0.index == 0
	if newPair {
		if stale, err := hs.linkGet(host); err == nil {
			if !ownedVeth(stale, br, eth0, podUID) {
				return created, fmt.Errorf("interface %s is not owned by Pod %s", host, podUID)
			}
			if err := hs.linkDel(stale.index); err != nil {
				return created, fmt.Errorf("remove stale %s: %w", host, err)
			}
		} else if !errors.Is(err, unix.ENODEV) {
			return created, err
		}
		f, err := os.Open(path)
		if err != nil {
			return created, err
		}
		err = hs.linkAddVeth(host, "eth0", n.mtu, f.Fd())
		f.Close()
		if err != nil {
			return created, fmt.Errorf("create veth %s: %w", host, err)
		}
	}
	hl, err := hs.linkGet(host)
	if err != nil {
		return created, fmt.Errorf("veth %s: %w", host, err)
	}
	// After a crash between veth creation and marking, the peer in this
	// Pod's namespace still proves which device belongs to the Pod.
	if !newPair && !ownedVeth(hl, br, eth0, podUID) {
		return created, fmt.Errorf("interface %s is not owned by Pod %s", host, podUID)
	}
	if hl.alias != vethAlias(podUID) {
		if err := hs.linkSetAlias(hl.index, vethAlias(podUID)); err != nil {
			return created, err
		}
	}
	if hl.master != br.index {
		// IPAM can give a new Pod the address of a Pod that just exited.
		// Discard the old MAC before this veth starts carrying traffic so
		// the next host-side TCP dial learns the new neighbor immediately.
		// Run this only while attaching, leaving live Pods' caches intact.
		if err := hs.forgetIPv4Neighbor(br.index, ip); err != nil {
			return created, fmt.Errorf("forget prior neighbor for Pod IP %s: %w", ip, err)
		}
		if err := hs.linkSetMaster(hl.index, br.index); err != nil {
			return created, fmt.Errorf("attach %s to %s: %w", host, n.Bridge, err)
		}
	}
	if !hl.up() {
		if err := hs.linkSetUp(hl.index); err != nil {
			return created, fmt.Errorf("bring up %s: %w", host, err)
		}
	}
	// Also apply when adopting an existing Pod after upgrade/restart.
	if err := hs.linkSetHairpin(hl.index); err != nil {
		return created, fmt.Errorf("enable Service hairpin on %s: %w", host, err)
	}
	err = inNetns(path, func(s *nlSock) error {
		lo, err := s.linkGet("lo")
		if err != nil {
			return fmt.Errorf("lo: %w", err)
		}
		if err := s.linkSetUp(lo.index); err != nil {
			return fmt.Errorf("bring up lo: %w", err)
		}
		eth, err := s.linkGet("eth0")
		if err != nil {
			return fmt.Errorf("eth0: %w", err)
		}
		if err := s.addrAdd(eth.index, netip.PrefixFrom(ip, n.PodCIDR.Bits())); err != nil {
			return fmt.Errorf("address %s: %w", ip, err)
		}
		if err := s.linkSetUp(eth.index); err != nil {
			return fmt.Errorf("bring up eth0: %w", err)
		}
		if err := s.routeAddDefault(n.Gateway(), eth.index); err != nil {
			return fmt.Errorf("default route: %w", err)
		}
		return nil
	})
	if err != nil {
		return created, fmt.Errorf("configure the pod's namespace: %w", err)
	}
	return created, nil
}

func (n *Net) ContainsTask(podUID string, pid uint32) (bool, error) {
	want, err := os.Stat(n.Path(podUID))
	if err != nil {
		return false, err
	}
	have, err := os.Stat(fmt.Sprintf("/proc/%d/ns/net", pid))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(want, have), nil
}

func (n *Net) Teardown(podUID string) error {
	if !IsUID(podUID) {
		return fmt.Errorf("network teardown: %q is not a pod uid", podUID)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	// The pair goes away with the namespace, unless a process is still inside
	// it. Deleting the host end first makes the result the same either way.
	s, err := nlOpen()
	if err != nil {
		return err
	}
	defer s.Close()
	l, err := s.linkGet(hostVeth(podUID))
	if err == nil {
		br, err := n.checkedBridge(s)
		if err != nil && !errors.Is(err, unix.ENODEV) {
			return err
		}
		var peer link
		if l.alias == "" && isNetns(n.Path(podUID)) {
			// Recover a veth left between creation and setting its alias.
			// The namespace mount outlives the task until we unmount it.
			if err := inNetns(n.Path(podUID), func(ps *nlSock) error {
				var err error
				peer, err = ps.linkGet("eth0")
				if errors.Is(err, unix.ENODEV) {
					return nil
				}
				return err
			}); err != nil {
				return err
			}
		}
		if !ownedVeth(l, br, peer, podUID) {
			return fmt.Errorf("refuse to remove a veth not owned by Pod %s", podUID)
		}
		if err := s.linkDel(l.index); err != nil && !errors.Is(err, unix.ENODEV) {
			return err
		}
	} else if !errors.Is(err, unix.ENODEV) {
		return err
	}
	return nsDelete(n.Path(podUID))
}

func (n *Net) Pods() ([]string, error) {
	entries, err := os.ReadDir(n.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if IsUID(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// PodAddrs returns the addresses eth0 has inside a pod's namespace (for checks).
func (n *Net) PodAddrs(podUID string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	err := inNetns(n.Path(podUID), func(s *nlSock) error {
		l, err := s.linkGet("eth0")
		if err != nil {
			return err
		}
		out, err = s.addrs(l.index)
		return err
	})
	return out, err
}

// ---- forwarding and address translation rules ----

// ensureRules sets up what the first checks of M2 showed to work on a host
// where Docker sets the FORWARD policy to DROP and bridged traffic passes
// through iptables (docs/m2-first-checks.md):
//
//   - pod to pod, pod to outside, and answers coming back, in our own chain,
//     reached from DOCKER-USER (the place Docker leaves alone) or, without
//     Docker, from FORWARD;
//   - masquerading for what leaves the pod range towards anything but pods and
//     services, in our own nat chain.
func (n *Net) ensureRules(ctx context.Context) error {
	for _, spec := range n.chainSpecs() {
		state, err := spec.read(ctx)
		if err != nil {
			return err
		}
		if err := spec.ensure(ctx, state); err != nil {
			return err
		}
	}
	parent := "FORWARD"
	if out, err := iptablesOutput(ctx, "-t", "filter", "-S", "DOCKER-USER"); err == nil {
		parent = "DOCKER-USER"
	} else if !missingChain(out) {
		return err
	}
	if err := ensureJump(ctx, "filter", parent, "-I", []string{"-j", forwardChain}); err != nil {
		return err
	}
	return ensureJump(ctx, "nat", "POSTROUTING", "-A", []string{"-s", n.PodCIDR.String(), "-j", postroutingName})
}
