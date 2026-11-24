package node

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"shearwater/internal/apis"
	"shearwater/internal/runtime"
)

func (a *Agent) podDir(uid string) string { return filepath.Join(a.DataDir, "pods", uid) }

// writeIfChanged leaves the file alone when it already has the content, so a
// pod that is fine causes no disk writes.
func writeIfChanged(path string, data []byte) error {
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, data) {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm() == 0644 {
			return nil
		}
		return os.Chmod(path, 0644)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	// These generated files are mounted as /etc/hosts and /etc/hostname.
	// They are not secrets: non-root application users must be able to read
	// them even when sw was launched with a private host umask.
	return os.Chmod(path, 0644)
}

// ensurePodFiles makes the pod's directory with the files every container
// gets mounted (hosts, hostname, resolv.conf) and prepares the volumes. It
// returns where each volume is on the host.
func (a *Agent) ensurePodFiles(pod *apis.Pod, ip string) (volumes map[string]string, err error) {
	uid := pod.Metadata.UID
	if !runtime.IsUID(uid) {
		return nil, fmt.Errorf("%q is not a pod uid", uid)
	}
	dir := a.podDir(uid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name := hostnameOf(pod)
	if err := writeIfChanged(filepath.Join(dir, "hostname"), []byte(name+"\n")); err != nil {
		return nil, err
	}
	hosts := "# Written by Shearwater for this pod.\n" +
		"127.0.0.1\tlocalhost\n" +
		"::1\tlocalhost ip6-localhost ip6-loopback\n" +
		ip + "\t" + name + "\n"
	if err := writeIfChanged(filepath.Join(dir, "hosts"), []byte(hosts)); err != nil {
		return nil, err
	}
	// Write resolv.conf once per pod according to dnsPolicy and dnsConfig
	// (docs/design-multinode-tenancy.md 4.2); existing pods are not rewritten.
	if resolv := filepath.Join(dir, "resolv.conf"); !exists(resolv) {
		var hostResolv []byte
		if pod.Spec.DNSPolicy == "Default" {
			hostResolv, _ = os.ReadFile("/etc/resolv.conf")
		}
		data := renderResolvConf(pod, a.dnsGateway(), hostResolv)
		if err := os.WriteFile(resolv, data, 0o644); err != nil {
			return nil, err
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "resolv.conf")); err != nil {
		return nil, err
	} else if info.Mode().Perm() != 0644 {
		if err = os.Chmod(filepath.Join(dir, "resolv.conf"), 0644); err != nil {
			return nil, err
		}
	}
	volumes = map[string]string{}
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.PersistentVolumeClaim != nil:
			p, err := a.claimDirectory(pod.Metadata.Namespace, v.PersistentVolumeClaim.ClaimName)
			if err != nil {
				return nil, fmt.Errorf("volume %q: %w", v.Name, err)
			}
			volumes[v.Name] = p
		case v.EmptyDir != nil:
			p := filepath.Join(dir, "volumes", v.Name)
			if !exists(p) {
				if err := os.MkdirAll(p, 0o777); err != nil {
					return nil, err
				}
				// Any user in any container may write here, whatever our umask is.
				if err := os.Chmod(p, 0o777); err != nil {
					return nil, err
				}
			}
			volumes[v.Name] = p
		case v.HostPath != nil:
			p, err := hostPath(v.HostPath)
			if err != nil {
				return nil, fmt.Errorf("volume %q: %w", v.Name, err)
			}
			volumes[v.Name] = p
		}
	}
	return volumes, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// dnsGateway is the node's bridge gateway IP (the first address of PodCIDR),
// where the in-process cluster DNS server listens on port 53.
func (a *Agent) dnsGateway() netip.Addr {
	cidr := a.PodCIDR
	if !cidr.IsValid() {
		cidr = apis.PodCIDR
	}
	return cidr.Masked().Addr().Next()
}

// renderResolvConf generates a pod's /etc/resolv.conf from its dnsPolicy,
// optional dnsConfig, the node's DNS gateway address and (for Default) the
// host's /etc/resolv.conf.
func renderResolvConf(pod *apis.Pod, gateway netip.Addr, hostResolv []byte) []byte {
	policy := pod.Spec.DNSPolicy
	cfg := pod.Spec.DNSConfig
	if policy == "Default" && cfg == nil {
		return hostResolv
	}

	var nameservers []string
	var searches []string
	var options []apis.PodDNSConfigOption

	switch policy {
	case "Default":
		nameservers, searches, options = parseResolvConf(hostResolv)
	case "None":
		// Start empty; only dnsConfig supplies entries.
	default:
		// "", "ClusterFirst", and "ClusterFirstWithHostNet" (hostNetwork is not supported).
		if gateway.IsValid() {
			nameservers = append(nameservers, gateway.String())
		}
		ns := pod.Metadata.Namespace
		if ns == "" {
			ns = "default"
		}
		searches = append(searches, ns+".svc.cluster.local", "svc.cluster.local", "cluster.local")
		five := "5"
		options = append(options, apis.PodDNSConfigOption{Name: "ndots", Value: &five})
	}

	if cfg != nil {
		for _, ns := range cfg.Nameservers {
			ns = strings.TrimSpace(ns)
			if ns != "" && !slices.Contains(nameservers, ns) {
				nameservers = append(nameservers, ns)
			}
		}
		for _, s := range cfg.Searches {
			s = strings.TrimSpace(s)
			if s != "" && !slices.Contains(searches, s) {
				searches = append(searches, s)
			}
		}
		for _, opt := range cfg.Options {
			name := strings.TrimSpace(opt.Name)
			if name == "" {
				continue
			}
			replaced := false
			for i := range options {
				if options[i].Name == name {
					options[i] = apis.PodDNSConfigOption{Name: name, Value: opt.Value}
					replaced = true
					break
				}
			}
			if !replaced {
				options = append(options, apis.PodDNSConfigOption{Name: name, Value: opt.Value})
			}
		}
	}

	var b strings.Builder
	b.WriteString("# Written by Shearwater for this pod.\n")
	if len(searches) > 0 {
		b.WriteString("search ")
		b.WriteString(strings.Join(searches, " "))
		b.WriteByte('\n')
	}
	for _, ns := range nameservers {
		b.WriteString("nameserver ")
		b.WriteString(ns)
		b.WriteByte('\n')
	}
	if len(options) > 0 {
		var parts []string
		for _, opt := range options {
			if opt.Value != nil && *opt.Value != "" {
				parts = append(parts, opt.Name+":"+*opt.Value)
			} else {
				parts = append(parts, opt.Name)
			}
		}
		b.WriteString("options ")
		b.WriteString(strings.Join(parts, " "))
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func parseResolvConf(data []byte) (nameservers, searches []string, options []apis.PodDNSConfigOption) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			if !slices.Contains(nameservers, fields[1]) {
				nameservers = append(nameservers, fields[1])
			}
		case "search", "domain":
			for _, s := range fields[1:] {
				if !slices.Contains(searches, s) {
					searches = append(searches, s)
				}
			}
		case "options":
			for _, tok := range fields[1:] {
				if k, v, ok := strings.Cut(tok, ":"); ok {
					val := v
					options = append(options, apis.PodDNSConfigOption{Name: k, Value: &val})
				} else {
					options = append(options, apis.PodDNSConfigOption{Name: tok})
				}
			}
		}
	}
	return nameservers, searches, options
}

// removePodFiles deletes the pod's directory (with its emptyDir volumes) and
// its logs. Both names are built from the uid, which is checked first.
func (a *Agent) removePodFiles(uid string) {
	if !runtime.IsUID(uid) {
		return
	}
	if err := removeUnder(filepath.Join(a.DataDir, "pods"), uid); err != nil {
		a.Logf("pod %s: %v", uid, err)
	}
	logs := filepath.Join(a.LogDir, "pods")
	entries, _ := os.ReadDir(logs)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_"+uid) {
			if err := removeUnder(logs, e.Name()); err != nil {
				a.Logf("pod %s: %v", uid, err)
			}
		}
	}
}

// removeUnder deletes root/name and what is in it. name must be one path
// element and root an absolute path, so a wrong value cannot reach outside.
func removeUnder(root, name string) error {
	if !filepath.IsAbs(root) || name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("refusing to remove %q under %q", name, root)
	}
	err := os.RemoveAll(filepath.Join(root, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// hostIP is the node's address: the source address of the default route. No
// packet is sent; connecting a UDP socket only asks the kernel for the route.
func hostIP() string {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}
