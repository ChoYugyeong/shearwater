package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	netpolChain   = "SHEARWATER-NETPOL"
	netpolEgress  = "SHEARWATER-NP-EGRESS"
	netpolIngress = "SHEARWATER-NP-INGRESS"
	netpolMarker  = "shearwater:netpol:v1"

	bridgeNFCallIPTablesPath = "/proc/sys/net/bridge/bridge-nf-call-iptables"
)

// NetpolPlan is the compiled node-local NetworkPolicy state for one node's
// PodCIDR. HasPolicies reports whether any NetworkPolicy object exists in the
// cluster; Pods holds only local Pods isolated for ingress and/or egress.
type NetpolPlan struct {
	HasPolicies bool
	Pods        []PodNetpol
}

// PodNetpol holds the compiled allow lists for one local Pod.
type PodNetpol struct {
	IP              netip.Addr
	IngressIsolated bool
	Ingress         []NetpolRule
	EgressIsolated  bool
	Egress          []NetpolRule
}

// NetpolRule is one allowed traffic tuple.
//   - Peer 0.0.0.0/0 matches any IPv4 peer (no -s/-d match is emitted).
//   - Protocol "" matches any protocol and port; "TCP" or "UDP" restricts it.
//   - Port 0 matches all ports of Protocol; EndPort > Port matches Port..EndPort.
type NetpolRule struct {
	Peer     netip.Prefix
	Protocol string
	Port     uint16
	EndPort  uint16
}

// KernelNetpol enforces NetworkPolicy rules in the filter table via three
// Shearwater-owned chains jumped from SHEARWATER-FORWARD. Filtering runs in
// FORWARD (after Service DNAT and before masquerading), so original source Pod
// IPs and backend container ports are visible while host traffic (edge proxy,
// node probes, gateway DNS on INPUT/OUTPUT) is unaffected.
type KernelNetpol struct {
	PodCIDR netip.Prefix

	// Injectable hooks for tests; nil uses the real Linux sysctl and modprobe.
	sysctlPath string
	modprobe   func(ctx context.Context) error

	last    string
	checked time.Time
}

func (k *KernelNetpol) validate(plan NetpolPlan) error {
	if !k.PodCIDR.IsValid() || !k.PodCIDR.Addr().Is4() {
		return fmt.Errorf("invalid pod CIDR %s", k.PodCIDR)
	}
	cidr := k.PodCIDR.Masked()
	seen := make(map[netip.Addr]bool, len(plan.Pods))
	for _, p := range plan.Pods {
		if !p.IP.Is4() || !cidr.Contains(p.IP) {
			return fmt.Errorf("invalid local Pod IP %s for range %s", p.IP, cidr)
		}
		if seen[p.IP] {
			return fmt.Errorf("duplicate local Pod IP %s in NetworkPolicy plan", p.IP)
		}
		seen[p.IP] = true
		for _, r := range p.Ingress {
			if err := validateNetpolRule(r); err != nil {
				return fmt.Errorf("Pod %s ingress rule: %w", p.IP, err)
			}
		}
		for _, r := range p.Egress {
			if err := validateNetpolRule(r); err != nil {
				return fmt.Errorf("Pod %s egress rule: %w", p.IP, err)
			}
		}
	}
	return nil
}

func validateNetpolRule(r NetpolRule) error {
	if !r.Peer.IsValid() || !r.Peer.Addr().Is4() {
		return fmt.Errorf("invalid peer CIDR %s", r.Peer)
	}
	switch r.Protocol {
	case "":
		if r.Port != 0 || r.EndPort != 0 {
			return fmt.Errorf("port %d:%d requires a protocol", r.Port, r.EndPort)
		}
	case "TCP", "UDP":
		if r.EndPort != 0 && (r.Port == 0 || r.EndPort < r.Port) {
			return fmt.Errorf("invalid port range %d:%d", r.Port, r.EndPort)
		}
	default:
		return fmt.Errorf("unsupported protocol %q", r.Protocol)
	}
	return nil
}

func netpolMarkerRule(chain string) string {
	return "-A " + chain + " -m comment --comment " + netpolMarker + " -j RETURN"
}

func writePortMatch(b *strings.Builder, r NetpolRule) {
	if r.Protocol == "" {
		return
	}
	fmt.Fprintf(b, " -p %s", strings.ToLower(r.Protocol))
	if r.Port != 0 {
		if r.EndPort > r.Port {
			fmt.Fprintf(b, " --dport %d:%d", r.Port, r.EndPort)
		} else {
			fmt.Fprintf(b, " --dport %d", r.Port)
		}
	}
}

// rules renders the atomic *filter iptables-restore script for plan.
// Every allow rule RETURNs so a same-node Pod-to-Pod packet must pass both the
// source Pod's egress rules and the destination Pod's ingress rules before
// returning to SHEARWATER-FORWARD to be accepted.
func (k *KernelNetpol) rules(plan NetpolPlan) string {
	pods := slices.Clone(plan.Pods)
	slices.SortFunc(pods, func(a, b PodNetpol) int { return a.IP.Compare(b.IP) })
	podCIDR := k.PodCIDR.Masked().String()

	var b strings.Builder
	fmt.Fprintf(&b, "*filter\n:%s - [0:0]\n:%s - [0:0]\n:%s - [0:0]\n", netpolChain, netpolEgress, netpolIngress)
	fmt.Fprintf(&b, "-A %s -m conntrack --ctstate RELATED,ESTABLISHED -m comment --comment %s -j RETURN\n", netpolChain, netpolMarker)
	fmt.Fprintf(&b, "-A %s -s %s -m comment --comment %s -j %s\n", netpolChain, podCIDR, netpolMarker, netpolEgress)
	fmt.Fprintf(&b, "-A %s -d %s -m comment --comment %s -j %s\n", netpolChain, podCIDR, netpolMarker, netpolIngress)
	fmt.Fprintln(&b, netpolMarkerRule(netpolChain))

	for _, p := range pods {
		if !p.EgressIsolated {
			continue
		}
		src := p.IP.String() + "/32"
		for _, r := range p.Egress {
			fmt.Fprintf(&b, "-A %s -s %s", netpolEgress, src)
			if r.Peer.Bits() > 0 {
				fmt.Fprintf(&b, " -d %s", r.Peer.Masked().String())
			}
			writePortMatch(&b, r)
			fmt.Fprintf(&b, " -m comment --comment %s -j RETURN\n", netpolMarker)
		}
		fmt.Fprintf(&b, "-A %s -s %s -m comment --comment %s -j DROP\n", netpolEgress, src, netpolMarker)
	}
	fmt.Fprintln(&b, netpolMarkerRule(netpolEgress))

	for _, p := range pods {
		if !p.IngressIsolated {
			continue
		}
		dst := p.IP.String() + "/32"
		for _, r := range p.Ingress {
			fmt.Fprintf(&b, "-A %s", netpolIngress)
			if r.Peer.Bits() > 0 {
				fmt.Fprintf(&b, " -s %s", r.Peer.Masked().String())
			}
			fmt.Fprintf(&b, " -d %s", dst)
			writePortMatch(&b, r)
			fmt.Fprintf(&b, " -m comment --comment %s -j RETURN\n", netpolMarker)
		}
		fmt.Fprintf(&b, "-A %s -d %s -m comment --comment %s -j DROP\n", netpolIngress, dst, netpolMarker)
	}
	fmt.Fprintln(&b, netpolMarkerRule(netpolIngress))
	fmt.Fprintln(&b, "COMMIT")
	return b.String()
}

// readNetpolChain inspects one owned chain in the filter table. Like
// readServiceChain, it refuses empty foreign chains and any rule outside our
// exact owned shapes.
func readNetpolChain(ctx context.Context, chain string) ([]string, error) {
	out, err := iptablesOutput(ctx, "-t", "filter", "-S", chain)
	if err != nil {
		if missingChain(out) {
			return nil, nil
		}
		return nil, err
	}
	rows := strings.Split(strings.TrimSpace(strings.ReplaceAll(out, "\"", "")), "\n")
	if len(rows) < 2 || rows[0] != "-N "+chain || rows[len(rows)-1] != netpolMarkerRule(chain) {
		return nil, fmt.Errorf("%s lacks the Shearwater NetworkPolicy ownership marker", chain)
	}
	for _, row := range rows[1:] {
		if !ownedNetpolRule(row, chain) {
			return nil, fmt.Errorf("%s contains a foreign rule", chain)
		}
	}
	return rows[1:], nil
}

// ownedNetpolRule validates both our generated rule syntax and the normalized
// form iptables -S prints back (such as inserted "-m tcp"/"-m udp").
func ownedNetpolRule(row, chain string) bool {
	f := strings.Fields(row)
	if len(f)%2 != 0 {
		return false
	}
	options := make(map[string]string)
	modules := make(map[string]bool)
	for i := 0; i < len(f); i += 2 {
		key, value := f[i], strings.Trim(f[i+1], "\"")
		if key == "-m" {
			if modules[value] || (value != "comment" && value != "conntrack" && value != "tcp" && value != "udp") {
				return false
			}
			modules[value] = true
		} else {
			if _, exists := options[key]; exists {
				return false
			}
			options[key] = value
		}
	}
	if options["-A"] != chain || !modules["comment"] || options["--comment"] != netpolMarker {
		return false
	}
	target := options["-j"]
	if target == "RETURN" && len(options) == 3 && len(modules) == 1 {
		return true
	}

	switch chain {
	case netpolChain:
		if target == "RETURN" && modules["conntrack"] && len(modules) == 2 && len(options) == 4 {
			st := options["--ctstate"]
			return st == "RELATED,ESTABLISHED" || st == "ESTABLISHED,RELATED"
		}
		if target == netpolEgress && len(modules) == 1 && len(options) == 4 {
			p, err := netip.ParsePrefix(options["-s"])
			return err == nil && p.Addr().Is4()
		}
		if target == netpolIngress && len(modules) == 1 && len(options) == 4 {
			p, err := netip.ParsePrefix(options["-d"])
			return err == nil && p.Addr().Is4()
		}
		return false

	case netpolEgress:
		src, err := netip.ParsePrefix(options["-s"])
		if err != nil || !src.Addr().Is4() || src.Bits() != 32 || modules["conntrack"] {
			return false
		}
		if target == "DROP" {
			return len(options) == 4 && len(modules) == 1
		}
		if target != "RETURN" {
			return false
		}
		return ownedNetpolAllowOptions(options, modules, "-d")

	case netpolIngress:
		dst, err := netip.ParsePrefix(options["-d"])
		if err != nil || !dst.Addr().Is4() || dst.Bits() != 32 || modules["conntrack"] {
			return false
		}
		if target == "DROP" {
			return len(options) == 4 && len(modules) == 1
		}
		if target != "RETURN" {
			return false
		}
		return ownedNetpolAllowOptions(options, modules, "-s")
	}
	return false
}

func ownedNetpolAllowOptions(options map[string]string, modules map[string]bool, peerFlag string) bool {
	optCount := 4 // -A, --comment, -j, and the local pod -s or -d
	modCount := 1 // comment
	if rawPeer, ok := options[peerFlag]; ok {
		p, err := netip.ParsePrefix(rawPeer)
		if err != nil || !p.Addr().Is4() {
			return false
		}
		optCount++
	}
	proto, hasProto := options["-p"]
	if hasProto {
		if proto != "tcp" && proto != "udp" {
			return false
		}
		optCount++
		if modules[proto] {
			modCount++
		}
		if dport, ok := options["--dport"]; ok {
			if !validIPTablesPortSpec(dport) {
				return false
			}
			optCount++
		}
	} else if _, hasDport := options["--dport"]; hasDport {
		return false
	}
	return len(options) == optCount && len(modules) == modCount
}

func validIPTablesPortSpec(s string) bool {
	if a, b, ok := strings.Cut(s, ":"); ok {
		p1, e1 := strconv.ParseUint(a, 10, 16)
		p2, e2 := strconv.ParseUint(b, 10, 16)
		return e1 == nil && e2 == nil && p1 >= 1 && p2 >= p1
	}
	p, err := strconv.ParseUint(s, 10, 16)
	return err == nil && p >= 1
}

func restoreNetpolRules(ctx context.Context, script string) error {
	var transaction strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(script), "\n") {
		if strings.HasPrefix(line, ":") {
			chain := strings.Fields(line)[0][1:]
			old, err := readNetpolChain(ctx, chain)
			if err != nil {
				return err
			}
			if old == nil {
				fmt.Fprintln(&transaction, "-N "+chain)
			} else {
				for _, rule := range old {
					fmt.Fprintln(&transaction, "-D "+strings.TrimPrefix(rule, "-A "))
				}
			}
		} else {
			fmt.Fprintln(&transaction, line)
		}
	}
	cmd := exec.CommandContext(ctx, "iptables-restore", "--wait", "2", "--noflush")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdin = strings.NewReader(transaction.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install NetworkPolicy rules: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureBridgeNetfilter makes same-node bridged Pod-to-Pod traffic pass through
// iptables FORWARD by loading br_netfilter (only when the sysctl file is
// missing) and setting net.bridge.bridge-nf-call-iptables=1.
func (k *KernelNetpol) ensureBridgeNetfilter(ctx context.Context) error {
	path := k.sysctlPath
	if path == "" {
		path = bridgeNFCallIPTablesPath
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		load := k.modprobe
		if load == nil {
			load = func(ctx context.Context) error {
				cmd := exec.CommandContext(ctx, "modprobe", "br_netfilter")
				cmd.Env = append(os.Environ(), "LC_ALL=C")
				if out, err := cmd.CombinedOutput(); err != nil {
					return fmt.Errorf("modprobe br_netfilter: %w: %s", err, strings.TrimSpace(string(out)))
				}
				return nil
			}
		}
		if err := load(ctx); err != nil {
			return fmt.Errorf("NetworkPolicy needs br_netfilter for same-node Pod traffic (%s missing): %w", path, err)
		}
		b, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("NetworkPolicy needs %s after loading br_netfilter: %w", path, err)
		}
	} else if err != nil {
		return fmt.Errorf("read %s for NetworkPolicy: %w", path, err)
	}
	if strings.TrimSpace(string(b)) != "1" {
		if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
			return fmt.Errorf("set %s=1 for NetworkPolicy: %w", path, err)
		}
	}
	return nil
}

// Reconcile programs or removes the node's NetworkPolicy iptables rules.
// When no NetworkPolicy exists in the cluster, any installed jump and chains
// are removed cleanly and br_netfilter is left untouched.
func (k *KernelNetpol) Reconcile(ctx context.Context, plan NetpolPlan) error {
	if err := k.validate(plan); err != nil {
		return err
	}
	if !plan.HasPolicies {
		if k.last == "" && !k.checked.IsZero() && time.Since(k.checked) < 30*time.Second {
			return nil
		}
		if err := k.Teardown(ctx); err != nil {
			return err
		}
		k.last, k.checked = "", time.Now()
		return nil
	}
	script := k.rules(plan)
	if script == k.last && time.Since(k.checked) < 30*time.Second {
		return nil
	}
	if err := k.ensureBridgeNetfilter(ctx); err != nil {
		return err
	}
	if err := restoreNetpolRules(ctx, script); err != nil {
		return err
	}
	if err := ensureJump(ctx, "filter", forwardChain, "-I", []string{"-j", netpolChain}); err != nil {
		return err
	}
	k.last, k.checked = script, time.Now()
	return nil
}

// Check performs a read-only ownership check on all three NetworkPolicy chains
// (suitable for Net.CheckFirewall / sw doctor).
func (k *KernelNetpol) Check(ctx context.Context) error {
	for _, c := range []string{netpolChain, netpolEgress, netpolIngress} {
		if _, err := readNetpolChain(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// Teardown removes the exact jump from SHEARWATER-FORWARD and all three
// Shearwater NetworkPolicy chains after verifying ownership of every rule.
func (k *KernelNetpol) Teardown(ctx context.Context) error {
	chains := []string{netpolChain, netpolEgress, netpolIngress}
	all := make([][]string, len(chains))
	for i, c := range chains {
		rows, err := readNetpolChain(ctx, c)
		if err != nil {
			return err
		}
		all[i] = rows
	}
	if out, err := iptablesOutput(ctx, "-t", "filter", "-S", forwardChain); err == nil {
		jumpLine := "-A " + forwardChain + " -j " + netpolChain
		for _, rule := range strings.Split(out, "\n") {
			if strings.TrimSpace(rule) != jumpLine {
				continue
			}
			if _, err := iptablesOutput(ctx, "-t", "filter", "-D", forwardChain, "-j", netpolChain); err != nil {
				return err
			}
		}
	} else if !missingChain(out) {
		return err
	}
	for i, c := range chains {
		if all[i] == nil {
			continue
		}
		for _, row := range all[i] {
			args := strings.Fields(row)
			args[0] = "-D"
			if _, err := iptablesOutput(ctx, append([]string{"-t", "filter"}, args...)...); err != nil {
				return err
			}
		}
		if _, err := iptablesOutput(ctx, "-t", "filter", "-X", c); err != nil {
			return err
		}
	}
	k.last = ""
	return nil
}
