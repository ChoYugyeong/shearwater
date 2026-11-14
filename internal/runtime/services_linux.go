package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

const serviceNAT = "SHEARWATER-SERVICES"
const serviceSNAT = "SHEARWATER-SVC-SNAT"
const serviceInput = "SHEARWATER-SVC-INPUT"
const serviceMarker = "shearwater:services:v1"

type ServiceBackend struct {
	UID     string
	Address netip.AddrPort
}

type ServiceRoute struct {
	UID      string
	Address  netip.AddrPort
	Backends []ServiceBackend
}

// KernelServices programs TCP DNAT rather than accepting TCP in the sw
// process. The application sockets and the NAT connection table therefore
// outlive an ordinary sw restart. Callers serialize Reconcile/ForgetPod; a
// retiring Pod's address must not be released until both have succeeded.
type KernelServices struct {
	PodCIDR, ServiceCIDR netip.Prefix
	last                 string
	checked              time.Time
}

func (k *KernelServices) validate(routes []ServiceRoute) error {
	seen := make(map[netip.AddrPort]bool)
	for _, r := range routes {
		if !IsUID(r.UID) || !r.Address.Addr().Is4() || !k.ServiceCIDR.Contains(r.Address.Addr()) || r.Address.Port() == 0 || seen[r.Address] {
			return fmt.Errorf("invalid or duplicate Service route %s", r.Address)
		}
		seen[r.Address] = true
		for _, b := range r.Backends {
			if !IsUID(b.UID) || !b.Address.Addr().Is4() || !k.PodCIDR.Contains(b.Address.Addr()) || b.Address.Port() == 0 {
				return fmt.Errorf("invalid Service backend %s", b.Address)
			}
		}
	}
	return nil
}

func markerRule(chain string) string {
	return "-A " + chain + " -m comment --comment " + serviceMarker + " -j RETURN"
}

// Only these three named chains are inspected. A name alone is not ownership:
// require our version marker and a recognizable comment on every rule. Never
// adopt an empty foreign chain or flush a built-in/shared chain.
func readServiceChain(ctx context.Context, table, chain string) ([]string, error) {
	out, err := iptablesOutput(ctx, "-t", table, "-S", chain)
	if err != nil {
		if missingChain(out) {
			return nil, nil
		}
		return nil, err
	}
	rows := strings.Split(strings.TrimSpace(strings.ReplaceAll(out, "\"", "")), "\n")
	if len(rows) < 2 || rows[0] != "-N "+chain || rows[len(rows)-1] != markerRule(chain) {
		return nil, fmt.Errorf("%s lacks the Shearwater Service ownership marker", chain)
	}
	for _, row := range rows[1:] {
		if !ownedServiceRule(row, chain) {
			return nil, fmt.Errorf("%s contains a foreign rule", chain)
		}
	}
	return rows[1:], nil
}

// Ownership includes the rule's shape, not merely a familiar comment. Reject
// extra matches, duplicate switches and unexpected targets before any edit.
// iptables -S sometimes inserts "-m tcp" or omits a default --packet 0.
func ownedServiceRule(row, chain string) bool {
	f := strings.Fields(row)
	if len(f)%2 != 0 {
		return false
	}
	options := make(map[string]string)
	modules := make(map[string]bool)
	for i := 0; i < len(f); i += 2 {
		key, value := f[i], strings.Trim(f[i+1], "\"")
		if key == "-m" {
			if modules[value] || (value != "comment" && value != "tcp" && value != "conntrack" && value != "statistic") {
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
	if options["-A"] != chain || !modules["comment"] {
		return false
	}
	comment, target := options["--comment"], options["-j"]
	if comment == serviceMarker {
		if target == "RETURN" {
			return len(options) == 3 && len(modules) == 1
		}
		if chain == serviceNAT && target == "DNAT" && options["-p"] == "tcp" && len(options) == 5 && len(modules) == 1 {
			ip, err := netip.ParseAddrPort(options["--to-destination"])
			return err == nil && ip.Addr().Is4() && ip.Port() == 9
		}
		if chain == serviceSNAT && target == "MASQUERADE" && options["-p"] == "tcp" && options["--ctstate"] == "DNAT" && len(options) == 7 && len(modules) == 2 && modules["conntrack"] {
			d, e1 := netip.ParsePrefix(options["-d"])
			o, e2 := netip.ParsePrefix(options["--ctorigdst"])
			return e1 == nil && e2 == nil && d.Addr().Is4() && o.Addr().Is4()
		}
		return false
	}
	prefix, err := netip.ParsePrefix(options["-d"])
	port, e2 := strconv.ParseUint(options["--dport"], 10, 16)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 32 || e2 != nil || port == 0 || options["-p"] != "tcp" || modules["conntrack"] {
		return false
	}
	moduleCount := 1
	if modules["tcp"] {
		moduleCount++
	}
	if uid, ok := strings.CutPrefix(comment, "shearwater:service:"); ok && IsUID(uid) {
		return len(modules) == moduleCount && ((chain == serviceNAT && target == "RETURN" && len(options) == 6) ||
			(chain == serviceInput && target == "REJECT" && options["--reject-with"] == "tcp-reset" && len(options) == 7))
	}
	uid, ok := strings.CutPrefix(comment, "shearwater:backend:")
	if !ok || !IsUID(uid) || chain != serviceNAT || target != "DNAT" {
		return false
	}
	dest, e3 := netip.ParseAddrPort(options["--to-destination"])
	if e3 != nil || !dest.Addr().Is4() || dest.Port() == 0 {
		return false
	}
	count := 7
	if modules["statistic"] {
		switch options["--mode"] {
		case "nth":
			every, e := strconv.ParseUint(options["--every"], 10, 32)
			if e != nil || every < 2 {
				return false
			}
			count += 2
			if packet, ok := options["--packet"]; ok {
				if packet != "0" {
					return false
				}
				count++
			}
		case "random": // recognize rules from earlier development builds for safe replacement
			prob, e := strconv.ParseFloat(options["--probability"], 64)
			if e != nil || !(prob > 0 && prob < 1) {
				return false
			}
			count += 2
		default:
			return false
		}
		moduleCount++
	}
	return len(modules) == moduleCount && len(options) == count
}

func serviceOwners(rows []string) (map[netip.Addr]string, error) {
	owners := make(map[netip.Addr]string)
	for _, row := range rows {
		f := strings.Fields(row)
		i := slices.Index(f, "--comment")
		if i < 0 || i+1 == len(f) || !strings.HasPrefix(f[i+1], "shearwater:service:") {
			continue
		}
		uid := strings.TrimPrefix(f[i+1], "shearwater:service:")
		d := slices.Index(f, "-d")
		if !IsUID(uid) || d < 0 || d+1 == len(f) {
			return nil, errors.New("invalid persisted Service ownership")
		}
		ip, err := netip.ParsePrefix(f[d+1])
		if err != nil || ip.Bits() != 32 {
			return nil, errors.New("invalid persisted Service address")
		}
		if old := owners[ip.Addr()]; old != "" && old != uid {
			return nil, errors.New("conflicting persisted Service ownership")
		}
		owners[ip.Addr()] = uid
	}
	return owners, nil
}

func serviceMatch(r ServiceRoute) string {
	return " -d " + r.Address.Addr().String() + "/32 -p tcp --dport " + strconv.Itoa(int(r.Address.Port()))
}

func (k *KernelServices) rules(routes []ServiceRoute, blocked map[netip.Addr]bool) (string, string) {
	var nat, filter strings.Builder
	fmt.Fprintf(&nat, "*nat\n:%s - [0:0]\n:%s - [0:0]\n", serviceNAT, serviceSNAT)
	fmt.Fprintf(&filter, "*filter\n:%s - [0:0]\n", serviceInput)
	for _, r := range routes {
		match := serviceMatch(r)
		if !blocked[r.Address.Addr()] {
			for i, b := range r.Backends {
				fmt.Fprintf(&nat, "-A %s%s", serviceNAT, match)
				if remaining := len(r.Backends) - i; remaining > 1 {
					// First rule takes every Nth new connection, the next
					// every (N-1)th of those remaining: A, B, C, ... in order.
					fmt.Fprintf(&nat, " -m statistic --mode nth --every %d --packet 0", remaining)
				}
				fmt.Fprintf(&nat, " -m comment --comment shearwater:backend:%s -j DNAT --to-destination %s\n", b.UID, b.Address)
			}
		}
		// The terminal rule retains the Service UID even with no endpoints.
		// UID changes must retire its old conntrack entries before IP reuse.
		fmt.Fprintf(&nat, "-A %s%s -m comment --comment shearwater:service:%s -j RETURN\n", serviceNAT, match, r.UID)
		// A Service with no endpoints must not reach an unrelated wildcard
		// host listener. Successful DNAT already changed the destination, so
		// this INPUT rule only sees traffic that still targets the VIP itself.
		fmt.Fprintf(&filter, "-A %s%s -m comment --comment shearwater:service:%s -j REJECT --reject-with tcp-reset\n", serviceInput, match, r.UID)
	}
	fmt.Fprintln(&nat, markerRule(serviceNAT))
	// Keep the DNAT expression registered even when the ready set is empty.
	// With iptables-nft, removing its last DNAT expression destroys the NAT
	// hook and invalidates existing mappings. This anchor is UNREACHABLE:
	// the unconditional RETURN immediately above always exits the chain.
	fmt.Fprintf(&nat, "-A %s -p tcp -m comment --comment %s -j DNAT --to-destination %s:9\n", serviceNAT, serviceMarker, k.PodCIDR.Addr().Next())
	fmt.Fprintln(&nat, markerRule(serviceNAT))
	// SNAT also handles a Pod connecting to its own Service (hairpin). Match
	// the ORIGINAL destination, so direct Pod traffic keeps its source IP.
	fmt.Fprintf(&nat, "-A %s -p tcp -d %s -m conntrack --ctorigdst %s --ctstate DNAT -m comment --comment %s -j MASQUERADE\n", serviceSNAT, k.PodCIDR, k.ServiceCIDR, serviceMarker)
	fmt.Fprintln(&nat, markerRule(serviceSNAT))
	fmt.Fprintln(&nat, "COMMIT")
	fmt.Fprintln(&filter, markerRule(serviceInput))
	fmt.Fprintln(&filter, "COMMIT")
	return nat.String(), filter.String()
}

func restoreServiceRules(ctx context.Context, script string) error {
	// Delete exact rules instead of declaring/flushing an existing chain.
	// If another administrator adds a rule after our inspection, it remains
	// untouched. All exact deletions and additions still commit together.
	var transaction strings.Builder
	table := ""
	for _, line := range strings.Split(strings.TrimSpace(script), "\n") {
		if strings.HasPrefix(line, "*") {
			table = strings.TrimPrefix(line, "*")
		}
		if strings.HasPrefix(line, ":") {
			chain := strings.Fields(line)[0][1:]
			old, err := readServiceChain(ctx, table, chain)
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
	// --noflush never flushes the surrounding table. No shell is involved.
	cmd := exec.CommandContext(ctx, "iptables-restore", "--wait", "2", "--noflush")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdin = strings.NewReader(transaction.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install Service rules: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (k *KernelServices) hooks() []chainSpec {
	return []chainSpec{
		{"nat", "PREROUTING", [][]string{{"-d", k.ServiceCIDR.String(), "-m", "comment", "--comment", serviceMarker, "-j", serviceNAT}}},
		{"nat", "OUTPUT", [][]string{{"-d", k.ServiceCIDR.String(), "-m", "comment", "--comment", serviceMarker, "-j", serviceNAT}}},
		{"nat", "POSTROUTING", [][]string{{"-d", k.PodCIDR.String(), "-m", "comment", "--comment", serviceMarker, "-j", serviceSNAT}}},
		{"filter", "INPUT", [][]string{{"-d", k.ServiceCIDR.String(), "-m", "comment", "--comment", serviceMarker, "-j", serviceInput}}},
	}
}

func (k *KernelServices) Reconcile(ctx context.Context, routes []ServiceRoute) error {
	if err := k.validate(routes); err != nil {
		return err
	}
	slices.SortFunc(routes, func(a, b ServiceRoute) int { return a.Address.Compare(b.Address) })
	nat, filter := k.rules(routes, nil)
	desired := nat + filter
	if nat+filter == k.last && time.Since(k.checked) < 30*time.Second {
		return nil
	}
	oldRows, err := readServiceChain(ctx, "nat", serviceNAT)
	if err != nil {
		return err
	}
	for _, c := range [][2]string{{"nat", serviceSNAT}, {"filter", serviceInput}} {
		if _, err := readServiceChain(ctx, c[0], c[1]); err != nil {
			return err
		}
	}
	old, err := serviceOwners(oldRows)
	if err != nil {
		return err
	}
	for ip := range old {
		if !k.ServiceCIDR.Contains(ip) {
			return errors.New("persisted Service address outside configured CIDR")
		}
	}
	wanted := make(map[netip.Addr]bool)
	owners := make(map[netip.Addr]string)
	for _, r := range routes {
		wanted[r.Address.Addr()] = true
	}
	attached, addressErr := (ServiceAddresses{CIDR: k.ServiceCIDR, retainStale: true}).Reconcile(ctx, wanted)
	if addressErr == nil && !maps.Equal(wanted, attached) {
		addressErr = errors.New("Service addresses are not ready")
	}
	// A route can become foreign while sw runs (for example, a new VPN).
	// Merely detaching the /32 is insufficient: stale DNAT would still
	// intercept that traffic. Withdraw unusable VIPs even on partial error.
	allowed := make([]ServiceRoute, 0, len(routes))
	for _, r := range routes {
		if attached[r.Address.Addr()] {
			allowed = append(allowed, r)
			owners[r.Address.Addr()] = r.UID
		}
	}
	routes = allowed
	nat, filter = k.rules(routes, nil)
	fence := make(map[netip.Addr]bool)
	for ip, uid := range old {
		if owners[ip] != uid {
			fence[ip] = true
		}
	}
	if err := restoreServiceRules(ctx, filter); err != nil {
		return err
	}
	if len(fence) > 0 {
		// Preserve OLD ownership while cleanup is pending. If sw dies or
		// netlink fails here, the next pass must retry the same fence, not
		// mistake the replacement UID for completed retirement.
		pending := slices.Clone(routes)
		for i := range pending {
			if fence[pending[i].Address.Addr()] {
				pending[i].UID = old[pending[i].Address.Addr()]
			}
		}
		for ip := range fence {
			if owners[ip] == "" {
				pending = append(pending, ServiceRoute{UID: old[ip], Address: netip.AddrPortFrom(ip, 1)})
			}
		}
		paused, _ := k.rules(pending, fence)
		if err := restoreServiceRules(ctx, paused); err != nil {
			return err
		}
		if err := k.forget(ctx, func(c serviceConnection) bool { return fence[c.service] }); err != nil {
			return err
		}
	}
	if err := restoreServiceRules(ctx, nat); err != nil {
		return err
	}
	for _, h := range k.hooks() {
		if _, err := iptablesOutput(ctx, append([]string{"-t", h.table, "-C", h.name}, h.rules[0]...)...); err == nil {
			continue
		}
		if _, err := iptablesOutput(ctx, append([]string{"-t", h.table, "-I", h.name, "1"}, h.rules[0]...)...); err != nil {
			return err
		}
	}
	// A removed VIP must never keep forwarding after observers see its
	// address detached. Finish address retirement only after DNAT withdrawal.
	_, detachErr := (ServiceAddresses{CIDR: k.ServiceCIDR}).Reconcile(ctx, wanted)
	if addressErr != nil || detachErr != nil {
		return errors.Join(addressErr, detachErr)
	}
	k.last, k.checked = desired, time.Now()
	return nil
}

// Teardown is exclusively for sw down --all, after stopping the server. A
// plain sw down intentionally leaves this data path and its connections alive.
func (k *KernelServices) Teardown(ctx context.Context) error {
	types := [][2]string{{"nat", serviceNAT}, {"nat", serviceSNAT}, {"filter", serviceInput}}
	all := make([][]string, len(types))
	for i, c := range types {
		rows, err := readServiceChain(ctx, c[0], c[1])
		if err != nil {
			return err
		}
		all[i] = rows
	}
	for _, h := range k.hooks() {
		if _, err := iptablesOutput(ctx, append([]string{"-t", h.table, "-C", h.name}, h.rules[0]...)...); err != nil {
			continue
		}
		if _, err := iptablesOutput(ctx, append([]string{"-t", h.table, "-D", h.name}, h.rules[0]...)...); err != nil {
			return err
		}
	}
	owners, err := serviceOwners(all[0])
	if err != nil {
		return err
	}
	if len(owners) > 0 {
		if err := k.forget(ctx, func(c serviceConnection) bool { return owners[c.service] != "" }); err != nil {
			return err
		}
	}
	for i, c := range types {
		if all[i] == nil {
			continue
		}
		for _, row := range all[i] {
			args := strings.Fields(row)
			args[0] = "-D"
			if _, err := iptablesOutput(ctx, append([]string{"-t", c[0]}, args...)...); err != nil {
				return err
			}
		}
		if _, err := iptablesOutput(ctx, "-t", c[0], "-X", c[1]); err != nil {
			return err
		}
	}
	return nil
}
