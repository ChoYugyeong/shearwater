package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

const bridgeAlias = "shearwater:pod-bridge"

func (n *Net) bridgeMAC() []byte {
	gw := n.Gateway().As4()
	return append([]byte{0x0a, 0x58}, gw[:]...)
}

// Names can collide with something the operator created. New links carry an
// explicit ownership marker. A pre-M6 bridge has no marker, so adopt it only
// if its kind, pinned MAC and only IPv4 address all match our old setup.
func (n *Net) checkedBridge(s *nlSock) (link, error) {
	br, err := s.linkGet(n.Bridge)
	if err != nil {
		return link{}, err
	}
	ips, err := s.addrs(br.index)
	if err != nil {
		return link{}, err
	}
	gw := netip.PrefixFrom(n.Gateway(), n.PodCIDR.Bits())
	ours := br.alias == bridgeAlias ||
		(br.alias == "" && bytes.Equal(br.addr, n.bridgeMAC()) && len(ips) == 1 && ips[0] == gw)
	if br.kind != "bridge" || !ours || len(ips) > 1 || (len(ips) == 1 && ips[0] != gw) {
		return link{}, fmt.Errorf("interface %s is not a Shearwater bridge", n.Bridge)
	}
	return br, nil
}

func checkedServiceLink(s *nlSock, cidr netip.Prefix) (link, error) {
	l, err := s.linkGet(serviceLink)
	if err != nil {
		return link{}, err
	}
	if l.kind != "dummy" || l.alias != serviceLinkAlias {
		return link{}, fmt.Errorf("interface %s is not a Shearwater dummy", serviceLink)
	}
	ips, err := s.addrs(l.index)
	if err != nil {
		return link{}, err
	}
	for _, p := range ips {
		if p.Bits() != 32 || !cidr.Contains(p.Addr()) {
			return link{}, fmt.Errorf("interface %s has an address outside Shearwater ownership: %s", serviceLink, p)
		}
	}
	return l, nil
}

// CheckHostRanges is read-only and usable by doctor. A local /32 always wins
// over a broader route, so even one overlapping host prefix would divert
// traffic away from its existing network. Only our own routes are expected.
func (n *Net) CheckHostRanges(ctx context.Context) error {
	s, err := nlOpenContext(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	br, err := n.checkedBridge(s)
	if err != nil && !errors.Is(err, unix.ENODEV) {
		return err
	}
	svc, err := checkedServiceLink(s, n.ServiceCIDR)
	if err != nil && !errors.Is(err, unix.ENODEV) {
		return err
	}
	routes, err := s.ipv4Routes()
	if err != nil {
		return err
	}
	for _, r := range routes {
		if r.prefix.Overlaps(n.PodCIDR) && !(br.index != 0 && r.index == br.index && r.prefix.Bits() >= n.PodCIDR.Bits()) {
			return fmt.Errorf("Pod range %s overlaps host route %s", n.PodCIDR, r.prefix)
		}
		if r.prefix.Overlaps(n.ServiceCIDR) && !(svc.index != 0 && r.index == svc.index && r.prefix.Bits() == 32 && r.kind == unix.RTN_LOCAL) {
			return fmt.Errorf("Service range %s overlaps host route %s", n.ServiceCIDR, r.prefix)
		}
	}
	addresses, err := s.ipv4Addresses()
	if err != nil {
		return err
	}
	for _, a := range addresses {
		if (n.PodCIDR.Contains(a.prefix.Addr()) && a.index != br.index) ||
			(n.ServiceCIDR.Contains(a.prefix.Addr()) && a.index != svc.index) {
			return fmt.Errorf("host interface %d already has address %s in a Shearwater range", a.index, a.prefix)
		}
	}
	return nil
}

func CheckIPv4Forwarding() error {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) != "1" {
		return errors.New("IPv4 forwarding is disabled")
	}
	return nil
}

// TeardownHost runs only under the node lock, after all owned Pods and
// namespace mounts have been stopped. It removes our links, chains and exact
// jump rules. It never flushes a shared table or changes a system-wide sysctl.
func (n *Net) TeardownHost(ctx context.Context) error {
	s, err := nlOpenContext(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	br, err := n.checkedBridge(s)
	if err != nil && !errors.Is(err, unix.ENODEV) {
		return err
	}
	if br.index != 0 {
		busy, err := s.linkHasPorts(br.index)
		if err != nil {
			return err
		}
		if busy {
			return fmt.Errorf("bridge %s still has attached interfaces", n.Bridge)
		}
	}
	svc, err := checkedServiceLink(s, n.ServiceCIDR)
	if err != nil && !errors.Is(err, unix.ENODEV) {
		return err
	}
	if err := n.removeRules(ctx); err != nil {
		return err
	}
	services := &KernelServices{PodCIDR: n.PodCIDR, ServiceCIDR: n.ServiceCIDR}
	if err := services.Teardown(ctx); err != nil {
		return err
	}
	for _, l := range []link{svc, br} {
		if l.index != 0 {
			if err := s.linkDel(l.index); err != nil && !errors.Is(err, unix.ENODEV) {
				return err
			}
		}
	}
	return nil
}

func iptablesOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "iptables", append([]string{"-w", "2"}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	b, err := cmd.CombinedOutput()
	if err != nil {
		return string(b), fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

type chainSpec struct {
	table, name string
	rules       [][]string
}

type chainState struct {
	exists  bool
	indices []int // exact known rules, in the order in which the kernel lists them
}

func (n *Net) chainSpecs() []chainSpec {
	br, pods, svcs := n.Bridge, n.PodCIDR.String(), n.ServiceCIDR.String()
	return []chainSpec{
		{"filter", forwardChain, [][]string{
			{"-i", br, "-o", br, "-j", "ACCEPT"},
			{"-i", br, "!", "-o", br, "-j", "ACCEPT"},
			{"-o", br, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
		}},
		{"nat", postroutingName, [][]string{
			{"-d", pods, "-j", "RETURN"},
			{"-d", svcs, "-j", "RETURN"},
			{"!", "-o", br, "-j", "MASQUERADE"},
		}},
	}
}

// Recognize legacy M2 chains by their exact rules. An interrupted setup may
// leave a subset; a rule we didn't write makes the entire chain foreign.
// Never use -F: even after inspection another program could add a rule.
func (c chainSpec) read(ctx context.Context) (chainState, error) {
	out, err := iptablesOutput(ctx, "-t", c.table, "-S", c.name)
	if err != nil {
		if missingChain(out) {
			return chainState{}, nil
		}
		return chainState{}, err
	}
	rows := strings.Split(strings.TrimSpace(out), "\n")
	if rows[0] != "-N "+c.name {
		return chainState{}, fmt.Errorf("%s is not a Shearwater user chain", c.name)
	}
	s := chainState{exists: true}
	for _, row := range rows[1:] {
		i := slices.IndexFunc(c.rules, func(rule []string) bool {
			return row == "-A "+c.name+" "+strings.Join(rule, " ")
		})
		if i < 0 || slices.Contains(s.indices, i) {
			return chainState{}, fmt.Errorf("iptables chain %s contains a rule Shearwater did not install", c.name)
		}
		s.indices = append(s.indices, i)
	}
	return s, nil
}

func (c chainSpec) ensure(ctx context.Context, state chainState) error {
	if !state.exists {
		if _, err := iptablesOutput(ctx, "-t", c.table, "-N", c.name); err != nil {
			return err
		}
	}
	// Restore a missing suffix without touching the prefix. If a prior
	// teardown stopped in the middle, rebuild using only exact deletions.
	n := 0
	for n < len(state.indices) && state.indices[n] == n {
		n++
	}
	if n != len(state.indices) {
		if err := c.deleteRules(ctx, state); err != nil {
			return err
		}
		n = 0
	}
	for _, rule := range c.rules[n:] {
		if _, err := iptablesOutput(ctx, append([]string{"-t", c.table, "-A", c.name}, rule...)...); err != nil {
			return err
		}
	}
	return nil
}

func (c chainSpec) deleteRules(ctx context.Context, state chainState) error {
	for _, i := range state.indices {
		if _, err := iptablesOutput(ctx, append([]string{"-t", c.table, "-D", c.name}, c.rules[i]...)...); err != nil {
			return err
		}
	}
	return nil
}

func ensureJump(ctx context.Context, table, parent, op string, rule []string) error {
	out, err := iptablesOutput(ctx, "-t", table, "-S", parent)
	if err != nil {
		return err
	}
	if slices.Contains(strings.Split(out, "\n"), "-A "+parent+" "+strings.Join(rule, " ")) {
		return nil
	}
	_, err = iptablesOutput(ctx, append([]string{"-t", table, op, parent}, rule...)...)
	return err
}

// CheckFirewall queries just the hook chains that Pod forwarding uses. It
// does not read Docker configuration or create any rule.
func (n *Net) CheckFirewall(ctx context.Context) error {
	for _, pair := range [][2]string{{"filter", "FORWARD"}, {"nat", "POSTROUTING"}} {
		if _, err := iptablesOutput(ctx, "-t", pair[0], "-S", pair[1]); err != nil {
			return err
		}
	}
	for _, spec := range n.chainSpecs() {
		if _, err := spec.read(ctx); err != nil {
			return err
		}
	}
	for _, c := range [][2]string{{"nat", serviceNAT}, {"nat", serviceSNAT}, {"filter", serviceInput}} {
		if _, err := readServiceChain(ctx, c[0], c[1]); err != nil {
			return err
		}
	}
	return nil
}

// Remove only our exact jumps. Check the current rules first so cleanup is
// idempotent and we can distinguish an absent chain from a command failure.
func (n *Net) removeRules(ctx context.Context) error {
	specs := n.chainSpecs()
	states := make([]chainState, len(specs))
	for i, spec := range specs {
		state, err := spec.read(ctx)
		if err != nil {
			return err
		}
		states[i] = state
	}
	for _, entry := range []struct {
		table, parent string
		rule          []string
		optional      bool
	}{
		{"filter", "DOCKER-USER", []string{"-j", forwardChain}, true},
		{"filter", "FORWARD", []string{"-j", forwardChain}, false},
		{"nat", "POSTROUTING", []string{"-s", n.PodCIDR.String(), "-j", postroutingName}, false},
	} {
		out, err := iptablesOutput(ctx, "-t", entry.table, "-S", entry.parent)
		if err != nil {
			if entry.optional && missingChain(out) {
				continue
			}
			return err
		}
		line := "-A " + entry.parent + " " + strings.Join(entry.rule, " ")
		for _, rule := range strings.Split(out, "\n") {
			if strings.TrimSpace(rule) != line {
				continue
			}
			if _, err := iptablesOutput(ctx, append([]string{"-t", entry.table, "-D", entry.parent}, entry.rule...)...); err != nil {
				return err
			}
		}
	}
	for i, spec := range specs {
		if !states[i].exists {
			continue
		}
		if err := spec.deleteRules(ctx, states[i]); err != nil {
			return err
		}
		// -X cannot remove a chain with a foreign rule or reference. Do
		// not try to remove either to make the command succeed.
		if _, err := iptablesOutput(ctx, "-t", spec.table, "-X", spec.name); err != nil {
			return err
		}
	}
	return nil
}

func missingChain(out string) bool {
	out = strings.ToLower(out)
	return strings.Contains(out, "no chain/target/match by that name") || strings.Contains(out, "chain does not exist")
}
