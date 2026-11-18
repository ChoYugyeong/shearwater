package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDirectTimeoutPreservesHostTCPSettings(t *testing.T) {
	// Include a custom established lifetime and a future state. The
	// serialized profile must carry both all the way to the kernel.
	host := tcpTimeouts{1: 115, 2: 59, 3: 17123, 4: 119, 5: 57, 6: 29,
		7: 120, 8: 9, 9: 117, 10: 299, 11: 297, 12: 425}
	want := maps.Clone(host)
	want[tcpTimeWait] = 30
	p := directTimeouts(host)
	if !maps.Equal(p, want) || host[tcpTimeWait] != 120 {
		t.Fatalf("direct profile %v, host %v", p, host)
	}
	raw := timeoutRequest(timeoutProtocol(), attr(timeoutData|unix.NLA_F_NESTED, p.data()))
	a, err := timeoutAttrs(nlMsg{data: raw})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseTCPTimeouts(a)
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("profile round trip: %v, %v", got, err)
	}
	delete(p, 3)
	a[timeoutData] = p.data()
	if _, err := parseTCPTimeouts(a); err == nil {
		t.Fatal("incomplete profile lost the established timeout")
	}
	for _, custom := range []uint32{0, 15, 30, 90, 240} {
		host[tcpTimeWait] = custom
		if directTimeouts(host) != nil || host[tcpTimeWait] != custom {
			t.Fatalf("overrode administrator's TIME_WAIT of %d", custom)
		}
	}
}

func TestDirectTimeoutRuleScopeAndOwnership(t *testing.T) {
	n := NewNet("", netip.MustParsePrefix("10.77.2.0/24"), netip.MustParsePrefix("10.88.0.0/16"))
	n.Bridge = "sw-test-br"
	p := tcpTimeouts{3: 12000, 7: 30}
	fingerprint := n.timeoutFingerprint(p)
	name, err := selectTimeoutName(fingerprint, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-s", "10.77.2.1/32", "-d", "10.77.2.0/24", "-o", "sw-test-br",
		"-p", "tcp", "-m", "tcp", "--syn", "-m", "comment", "--comment", "shearwater:direct-pod-tcp:" + fingerprint,
		"-j", "CT", "--timeout", name}
	if got := n.timeoutRule(name, fingerprint); !slices.Equal(got, want) {
		t.Fatalf("direct TCP match: %q", got)
	}
	if len(name) != 4 || fingerprint != n.timeoutFingerprint(tcpTimeouts{7: 30, 3: 12000}) {
		t.Fatal("unstable fingerprint or name truncated by xt_CT readback", name)
	}
	p[3]++
	if n.timeoutFingerprint(p) == fingerprint {
		t.Fatal("adopted a profile with a different established timeout")
	}
	p[3]--
	n.Bridge = "other"
	if n.timeoutFingerprint(p) == fingerprint {
		t.Fatal("adopted another bridge's profile")
	}
}

func TestDirectTimeoutNameCollisionAndRestart(t *testing.T) {
	const fp = "sw-pod-ffff0011223344556677"
	// Even an identically configured foreign timeout takes precedence over a
	// candidate name. Selection wraps and also respects pending reservations.
	kernel := map[string]string{"ffff": fp, "0000": ""}
	receipts := map[string]string{"0001": "another profile"}
	name, err := selectTimeoutName(fp, receipts, kernel)
	if err != nil || name != "0002" {
		t.Fatalf("short name collision: %q, %v", name, err)
	}
	receipts[name] = fp
	for _, objects := range []map[string]string{nil, kernel} {
		recovered, err := selectTimeoutName(fp, receipts, objects)
		if err != nil || recovered != name {
			t.Fatalf("interrupted or repeated setup: %q, %v", recovered, err)
		}
	}
}

func TestDirectTimeoutFourByteRuleReadback(t *testing.T) {
	// Exercise the ordinary -C/-A/-D code without privileges. Model a kernel
	// that accepts a full name at insertion but exposes four bytes on readback.
	dir := t.TempDir()
	script := `#!/bin/sh
set -eu
[ "$1 $2 $3 $4 $6" = "-w 2 -t raw OUTPUT" ] || exit 2
op=$5
shift 6
case "$op" in
    -S) cat "$SW_CT_TEST_RULES" ;;
    -A)
        args="$*"
        name=${args##* }
        printf '%s %.4s\n' "-A OUTPUT ${args% *}" "$name" >> "$SW_CT_TEST_RULES"
        ;;
    -C) grep -Fqx -- "-A OUTPUT $*" "$SW_CT_TEST_RULES" ;;
    -D)
        awk -v rule="-A OUTPUT $*" '$0 == rule && !seen { seen=1; next } { print } END { if (!seen) exit 1 }' "$SW_CT_TEST_RULES" > "$SW_CT_TEST_RULES.new"
        mv "$SW_CT_TEST_RULES.new" "$SW_CT_TEST_RULES"
        ;;
    *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "iptables"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rules")
	const foreign = "-P OUTPUT ACCEPT\n-A OUTPUT -d 192.0.2.11/32 -j RETURN\n"
	if err := os.WriteFile(path, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SW_CT_TEST_RULES", path)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	newNet := func() *Net {
		return NewNet(dir, netip.MustParsePrefix("10.77.2.0/24"), netip.MustParsePrefix("10.88.0.0/16"))
	}
	p := tcpTimeouts{3: 12345, tcpTimeWait: podTimeWaitSeconds}
	fingerprint := newNet().timeoutFingerprint(p)
	name, err := selectTimeoutName(fingerprint, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		n := newNet()
		installed, err := n.installTimeout(t.Context(), nil, name, fingerprint, p, true)
		if err != nil || !installed {
			t.Fatalf("install and verify: %v, %v", installed, err)
		}
		b, err := os.ReadFile(path)
		if err != nil || strings.Count(string(b), fingerprint) != 1 || !strings.HasSuffix(string(b), "--timeout "+name+"\n") {
			t.Fatalf("rule after reconciliation: %q, %v", b, err)
		}
	}
	for range 2 {
		if err := newNet().removeTimeoutRules(t.Context(), name, fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != foreign {
		t.Fatalf("teardown changed foreign rules: %q, %v", b, err)
	}
}

// Install, restart and tear down on an existing bridge name in a private
// namespace. A caller can opt into the live kernel test independently of the
// ordinary unprivileged suite. The test requires the scoped-policy feature.
func TestDirectPodTimeoutInPrivateNetns(t *testing.T) {
	if os.Getenv("SW_TEST_SERVICE_NETNS") != "1" {
		t.Skip("set SW_TEST_SERVICE_NETNS=1 in isolated privileged Linux QA")
	}
	ctx := t.Context()
	runDir := t.TempDir()
	err := onLockedThread(func() error {
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			return err
		}
		n := NewNet(runDir, netip.MustParsePrefix("10.244.0.0/24"), netip.MustParsePrefix("10.96.0.0/16"))
		if _, err := iptablesOutput(ctx, "-t", "raw", "-A", "OUTPUT", "-d", "192.0.2.77/32", "-j", "RETURN"); err != nil {
			return err
		}
		readRules := func() (string, error) { return iptablesOutput(ctx, "-t", "raw", "-S", "OUTPUT") }
		s, err := nlOpenProtocol(ctx, unix.NETLINK_NETFILTER)
		if err != nil {
			return err
		}
		defer s.Close()
		readDefaults := func() ([]byte, error) {
			rows, err := s.request(ctTimeoutDefaultGet, 0, false, timeoutRequest(timeoutProtocol()))
			if err != nil {
				return nil, err
			}
			if len(rows) != 1 {
				return nil, errors.New("missing default policy")
			}
			return rows[0].data, nil
		}
		defaults, err := readDefaults()
		if err != nil {
			return err
		}
		// Preinstall precisely the profile we want under the short name we
		// would select, but with no ownership receipt and a site's own rule.
		// Even an identical profile must survive as foreign.
		a, err := timeoutAttrs(nlMsg{data: defaults})
		if err != nil {
			return err
		}
		host, err := parseTCPTimeouts(a)
		if err != nil {
			return err
		}
		desired := directTimeouts(host)
		if desired == nil {
			return fmt.Errorf("fixture requires stock TIME_WAIT, found %d", host[tcpTimeWait])
		}
		foreign, err := selectTimeoutName(n.timeoutFingerprint(desired), nil, nil)
		if err != nil {
			return err
		}
		if _, err := s.request(ctTimeoutNew, unix.NLM_F_CREATE|unix.NLM_F_EXCL, false,
			timeoutRequest(attrStr(timeoutName, foreign), timeoutProtocol(), attr(timeoutData|unix.NLA_F_NESTED, desired.data()))); err != nil {
			return err
		}
		if _, err := iptablesOutput(ctx, "-t", "raw", "-A", "OUTPUT", "-d", "192.0.2.80/32", "-p", "tcp", "-j", "CT", "--timeout", foreign); err != nil {
			return err
		}
		before, err := readRules()
		if err != nil {
			return err
		}
		for range 3 {
			if err := n.podTimeouts(ctx, false); err != nil {
				return err
			}
			out, err := readRules()
			if err != nil || strings.Count(out, "shearwater:direct-pod-tcp") != 1 || !strings.HasPrefix(out, before) {
				return fmt.Errorf("expected one scoped rule after the site's rules: %q (%v)", out, err)
			}
			fields := strings.Fields(strings.TrimSpace(out))
			if name := fields[len(fields)-1]; len(name) != 4 || name == foreign {
				return fmt.Errorf("expected a four-byte timeout distinct from the site's %s, got %s", foreign, name)
			}
			n = NewNet(runDir, n.PodCIDR, n.ServiceCIDR)
		}
		for range 2 {
			if err := n.podTimeouts(ctx, true); err != nil {
				return err
			}
		}
		after, err := readRules()
		if err != nil || after != before {
			return fmt.Errorf("foreign raw rules changed: %q (%v)", after, err)
		}
		rows, err := s.request(ctTimeoutGet, 0, true, timeoutRequest())
		if err != nil || len(rows) != 1 {
			return fmt.Errorf("expected only the foreign named timeout, got %d (%v)", len(rows), err)
		}
		a, err = timeoutAttrs(rows[0])
		if err != nil || string(a[timeoutName]) != foreign+"\x00" {
			return fmt.Errorf("foreign named timeout was replaced: %v", err)
		}
		afterDefaults, err := readDefaults()
		if err != nil || !bytes.Equal(afterDefaults, defaults) {
			return fmt.Errorf("host TCP defaults changed: %v", err)
		}
		receipts, err := n.timeoutReceipts()
		if err != nil || len(receipts) != 0 {
			return fmt.Errorf("ownership receipts left after teardown: %v (%v)", receipts, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
