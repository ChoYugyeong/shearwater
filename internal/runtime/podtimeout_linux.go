package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Linux UAPI: nfnetlink.h and nfnetlink_cttimeout.h. Only named policies
// may be created/deleted. The host default is queried and never written.
const (
	ctTimeoutNew        = 8 << 8
	ctTimeoutGet        = ctTimeoutNew + 1
	ctTimeoutDelete     = ctTimeoutNew + 2
	ctTimeoutDefaultGet = ctTimeoutNew + 4

	timeoutName = 1
	timeoutL3   = 2
	timeoutL4   = 3
	timeoutData = 4
	tcpTimeWait = 7

	podTimeWaitSeconds   = 30
	linuxTimeWaitSeconds = 120
)

// These are conntrack state lifetimes, independent of TCP socket TIME_WAIT.
// FIN_WAIT, LAST_ACK, half-closed and established flows retain the host's
// lifetimes. Only TIME_WAIT on the direct local path ages sooner.
type tcpTimeouts map[uint16]uint32

func (p tcpTimeouts) data() []byte {
	var data []byte
	for _, state := range slices.Sorted(maps.Keys(p)) {
		data = append(data, attr(state, binary.BigEndian.AppendUint32(nil, p[state]))...)
	}
	return data
}

func (n *Net) timeoutFingerprint(p tcpTimeouts) string {
	// Authenticate the profile with its settings and the path it covers.
	h := sha256.New()
	fmt.Fprintf(h, "shearwater:direct-pod-tcp:v1/%s/%s/", n.Bridge, n.PodCIDR)
	h.Write(p.data())
	return fmt.Sprintf("sw-pod-%x", h.Sum(nil)[:10])
}

func timeoutRequest(attrs ...[]byte) []byte {
	return append([]byte{unix.AF_UNSPEC, 0, 0, 0}, bytes.Join(attrs, nil)...)
}

func timeoutProtocol() []byte {
	return append(attr(timeoutL3, binary.BigEndian.AppendUint16(nil, unix.AF_INET)), attr(timeoutL4, []byte{unix.IPPROTO_TCP})...)
}

func timeoutAttrs(m nlMsg) (map[uint16][]byte, error) {
	if len(m.data) < 4 {
		return nil, errors.New("short conntrack timeout response")
	}
	return checkedAttrs(m.data[4:])
}

func parseTCPTimeouts(a map[uint16][]byte) (tcpTimeouts, error) {
	if !bytes.Equal(a[timeoutL3], []byte{0, unix.AF_INET}) || !bytes.Equal(a[timeoutL4], []byte{unix.IPPROTO_TCP}) {
		return nil, errors.New("expected an IPv4 TCP timeout policy")
	}
	attrs, err := checkedAttrs(a[timeoutData])
	if err != nil {
		return nil, err
	}
	p := tcpTimeouts{}
	for state, value := range attrs {
		if len(value) != 4 {
			return nil, fmt.Errorf("invalid timeout for TCP state %d", state)
		}
		p[state] = binary.BigEndian.Uint32(value)
	}
	// From SYN_SENT through UNACK. Preserve additional states on newer kernels.
	for state := uint16(1); state <= 11; state++ {
		if _, ok := p[state]; !ok {
			return nil, fmt.Errorf("missing conntrack timeout for TCP state %d", state)
		}
	}
	return p, nil
}

func directTimeouts(defaults tcpTimeouts) tcpTimeouts {
	// An administrator's explicit non-default TIME_WAIT takes precedence,
	// whether longer or shorter. In that case the ordinary host policy applies.
	if defaults[tcpTimeWait] != linuxTimeWaitSeconds {
		return nil
	}
	p := maps.Clone(defaults)
	p[tcpTimeWait] = podTimeWaitSeconds
	return p
}

func (n *Net) timeoutRule(name, fingerprint string) []string {
	// raw OUTPUT sees the original destination before DNAT. Only initial
	// SYNs routed out our bridge from its own gateway can create a template.
	// Append after existing raw rules so an operator's CT template wins.
	return []string{"-s", n.Gateway().String() + "/32", "-d", n.PodCIDR.String(), "-o", n.Bridge,
		"-p", "tcp", "-m", "tcp", "--syn", "-m", "comment", "--comment", "shearwater:direct-pod-tcp:" + fingerprint,
		"-j", "CT", "--timeout", name}
}

func (n *Net) hasTimeoutRule(ctx context.Context, name, fingerprint string) (bool, error) {
	out, err := iptablesOutput(ctx, append([]string{"-t", "raw", "-C", "OUTPUT"}, n.timeoutRule(name, fingerprint)...)...)
	if err == nil {
		return true, nil
	}
	var status *exec.ExitError
	if errors.As(err, &status) && status.ExitCode() == 1 && (strings.TrimSpace(out) == "" ||
		strings.Contains(out, "does a matching rule exist") || missingChain(out) || strings.Contains(strings.ToLower(out), "no such file or directory")) {
		return false, nil
	}
	return false, err
}

func (n *Net) installTimeout(ctx context.Context, s *nlSock, name, fingerprint string, desired tcpTimeouts, exists bool) (bool, error) {
	if exists {
		has, err := n.hasTimeoutRule(ctx, name, fingerprint)
		if err != nil || has {
			return has, err
		}
		// If a backend cannot faithfully compare a tagged rule, appending on
		// every restart would duplicate it. Never replace an edited site rule.
		out, err := iptablesOutput(ctx, "-t", "raw", "-S", "OUTPUT")
		if err != nil {
			return false, err
		}
		if strings.Contains(out, "shearwater:direct-pod-tcp:"+fingerprint) {
			return false, fmt.Errorf("existing tagged direct Pod rule for %s does not match its exact definition", name)
		}
	} else {
		_, err := s.request(ctTimeoutNew, unix.NLM_F_CREATE|unix.NLM_F_EXCL, false,
			timeoutRequest(attrStr(timeoutName, name), timeoutProtocol(), attr(timeoutData|unix.NLA_F_NESTED, desired.data())))
		if err != nil {
			return false, fmt.Errorf("create exclusive named policy %s: %w", name, err)
		}
	}
	_, err := iptablesOutput(ctx, append([]string{"-t", "raw", "-A", "OUTPUT"}, n.timeoutRule(name, fingerprint)...)...)
	if err != nil {
		if !exists {
			_, rollback := s.request(ctTimeoutDelete, 0, false, timeoutRequest(attrStr(timeoutName, name)))
			if rollback != nil {
				return false, errors.Join(err, rollback)
			}
		}
		n.warnPodTimeout("could not attach the scoped CT timeout; retaining existing firewall policy", err)
		return false, nil
	}
	has, err := n.hasTimeoutRule(ctx, name, fingerprint)
	if err != nil || !has {
		return false, fmt.Errorf("cannot verify newly installed direct Pod rule for %s (present=%t, error=%v)", name, has, err)
	}
	return true, nil
}

func (n *Net) removeTimeoutRules(ctx context.Context, name, fingerprint string) error {
	for {
		has, err := n.hasTimeoutRule(ctx, name, fingerprint)
		if err != nil || !has {
			return err
		}
		if _, err := iptablesOutput(ctx, append([]string{"-t", "raw", "-D", "OUTPUT"}, n.timeoutRule(name, fingerprint)...)...); err != nil {
			return err
		}
	}
}

func deleteTimeout(s *nlSock, name string) error {
	// The kernel refuses removal while another rule references this profile.
	_, err := s.request(ctTimeoutDelete, 0, false, timeoutRequest(attrStr(timeoutName, name)))
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// Older xt_CT revisions expose only four bytes of timeout[] in rule dumps
// (usersize uses the v0 structure). Both -C and -D compare that dump. Use a
// four-byte real kernel name; keep the full fingerprint in the rule comment
// and an atomic, boot-local ownership receipt in the node's run directory.
const timeoutReceiptPrefix = ".direct-pod-ct-"

func (n *Net) receiptPath(name, fingerprint string) string {
	return filepath.Join(filepath.Dir(n.Dir), timeoutReceiptPrefix+name+"-"+fingerprint)
}

func (n *Net) timeoutReceipts() (map[string]string, error) {
	receipts := map[string]string{}
	entries, err := os.ReadDir(filepath.Dir(n.Dir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, entry := range entries {
		id, ok := strings.CutPrefix(entry.Name(), timeoutReceiptPrefix)
		if !ok {
			continue
		}
		name, fingerprint, ok := strings.Cut(id, "-")
		digest, prefixed := strings.CutPrefix(fingerprint, "sw-pod-")
		_, nameErr := hex.DecodeString(name)
		_, hashErr := hex.DecodeString(digest)
		if !ok || !prefixed || len(name) != 4 || len(digest) != 20 || nameErr != nil || hashErr != nil ||
			!entry.Type().IsRegular() || receipts[name] != "" {
			return nil, fmt.Errorf("invalid direct Pod timeout ownership receipt %q", entry.Name())
		}
		receipts[name] = fingerprint
	}
	return receipts, nil
}

func selectTimeoutName(fingerprint string, receipts, kernel map[string]string) (string, error) {
	for name, saved := range receipts {
		if saved == fingerprint {
			return name, nil
		}
	}
	digest, ok := strings.CutPrefix(fingerprint, "sw-pod-")
	if !ok || len(digest) != 20 {
		return "", errors.New("invalid direct Pod timeout fingerprint")
	}
	start, err := strconv.ParseUint(digest[:4], 16, 16)
	if err != nil {
		return "", err
	}
	for i := range 1 << 16 {
		name := fmt.Sprintf("%04x", uint16(start)+uint16(i))
		if _, taken := kernel[name]; !taken && receipts[name] == "" {
			return name, nil
		}
	}
	return "", errors.New("no available four-byte conntrack timeout name")
}

// podTimeouts installs once on an existing owned bridge, rechecks on host
// reconciliation, and removes only this bridge's exact rule and profiles.
func (n *Net) podTimeouts(ctx context.Context, remove bool) error {
	s, err := nlOpenProtocol(ctx, unix.NETLINK_NETFILTER)
	if err != nil {
		return err
	}
	defer s.Close()

	// Dump named timeout objects, never the connections in the flow table.
	rows, err := s.request(ctTimeoutGet, 0, true, timeoutRequest())
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPROTONOSUPPORT) || errors.Is(err, unix.ENOENT) {
		if !remove {
			n.warnPodTimeout("kernel named conntrack timeouts unavailable; using host TCP timeouts", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	kernel := map[string]string{}
	for _, row := range rows {
		a, err := timeoutAttrs(row)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(string(a[timeoutName]), "\x00")
		kernel[name] = "" // any family/protocol occupies this name
		if p, err := parseTCPTimeouts(a); err == nil && p[tcpTimeWait] == podTimeWaitSeconds {
			kernel[name] = n.timeoutFingerprint(p)
		}
	}
	receipts, err := n.timeoutReceipts()
	if err != nil {
		return err
	}
	for name, saved := range receipts {
		if actual, exists := kernel[name]; exists && actual != saved {
			return fmt.Errorf("conntrack timeout %q differs from its ownership receipt; preserving it", name)
		}
	}
	var wanted string
	if !remove {
		defaults, err := s.request(ctTimeoutDefaultGet, 0, false, timeoutRequest(timeoutProtocol()))
		if err != nil {
			return err
		}
		if len(defaults) != 1 {
			return errors.New("missing IPv4 TCP default timeout response")
		}
		a, err := timeoutAttrs(defaults[0])
		if err != nil {
			return err
		}
		p, err := parseTCPTimeouts(a)
		if err != nil {
			return err
		}
		if desired := directTimeouts(p); desired != nil {
			fingerprint := n.timeoutFingerprint(desired)
			wanted, err = selectTimeoutName(fingerprint, receipts, kernel)
			if err != nil {
				return err
			}
			path := n.receiptPath(wanted, fingerprint)
			if receipts[wanted] == "" {
				// Reserve before creating kernel state. The filename is the
				// complete receipt, including across a crash before rule install.
				f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}
				if err := f.Close(); err != nil {
					return err
				}
			}
			_, exists := kernel[wanted]
			installed, err := n.installTimeout(ctx, s, wanted, fingerprint, desired, exists)
			if errors.Is(err, unix.EEXIST) {
				// Another owner obtained the kernel name following our dump.
				return errors.Join(err, os.Remove(path))
			}
			if err != nil || !installed {
				return err
			}
		}
		n.timeoutWarning = ""
	}
	for name, fingerprint := range receipts {
		if name == wanted {
			continue
		}
		if _, exists := kernel[name]; exists {
			if err := n.removeTimeoutRules(ctx, name, fingerprint); err != nil {
				return err
			}
			if err := deleteTimeout(s, name); err != nil {
				return err
			}
		}
		if err := os.Remove(n.receiptPath(name, fingerprint)); err != nil {
			return err
		}
	}
	return nil
}

func (n *Net) warnPodTimeout(message string, err error) {
	warning := fmt.Sprintf("%s: %v", message, err)
	if n.timeoutWarning != warning {
		log.Printf("direct Pod TCP: %s", warning)
		n.timeoutWarning = warning
	}
}
