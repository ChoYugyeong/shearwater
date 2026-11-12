package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"

	"shearwater/internal/apis"
	"shearwater/internal/runtime"
)

type hostCheck struct {
	name, detail, hint string
	err                error
}

func dataFilesystem(path string) error {
	// statfs needs an existing path. Find the nearest existing ancestor
	// without creating anything, even on a machine that has never run sw.
	for {
		var st unix.Statfs_t
		if err := unix.Statfs(path, &st); err == nil {
			switch st.Type {
			case 0x01021997, unix.CIFS_SUPER_MAGIC, unix.NFS_SUPER_MAGIC: // 9p (WSL Windows mounts), SMB, NFS
				return fmt.Errorf("%s is on a shared filesystem unsuitable for the SQLite WAL", path)
			}
			return nil
		} else if !errors.Is(err, os.ErrNotExist) || path == "/" {
			return err
		}
		path = filepath.Dir(path)
	}
}

func hostChecks(ctx context.Context, o opsOptions) []hostCheck {
	var out []hostCheck
	add := func(name, detail, hint string, err error) {
		out = append(out, hostCheck{name, detail, hint, err})
	}
	add("SQLite filesystem", o.DataDir, "put --data-dir on the Linux filesystem, for example /var/lib/shearwater", dataFilesystem(o.DataDir))
	if o.Runtime == "none" {
		return out
	}
	info, err := os.Stat(o.Socket)
	if err == nil && info.Mode()&os.ModeSocket == 0 {
		err = fmt.Errorf("%s is not a socket", o.Socket)
	}
	version := ""
	if err == nil {
		rt := runtime.NewContainerd(o.Socket)
		version, err = rt.Version(ctx)
		rt.Close()
	}
	add("containerd", strings.TrimSpace(o.Socket+" "+version),
		"start your existing containerd or use --containerd-socket=/path/to/containerd.sock", err)
	for _, binary := range []string{"runc", "iptables"} {
		path, err := exec.LookPath(binary)
		add(binary, path, "make "+binary+" available in the WSL Linux PATH", err)
	}
	var cgroups unix.Statfs_t
	err = unix.Statfs("/sys/fs/cgroup", &cgroups)
	if err == nil && cgroups.Type != unix.CGROUP2_SUPER_MAGIC {
		err = errors.New("cgroup v2 is not mounted")
	}
	add("cgroup v2", "/sys/fs/cgroup", "use the WSL Ubuntu-24.04 Linux runtime with cgroup v2 enabled", err)
	add("IPv4 forwarding", "enabled", "enable net.ipv4.ip_forward on the host before starting Pod networking", runtime.CheckIPv4Forwarding())
	n := runtime.NewNet(o.RunDir, apis.PodCIDR, apis.ServiceCIDR)
	add("network ranges", apis.PodCIDR.String()+" and "+apis.ServiceCIDR.String(),
		"inspect ip -4 route show table all; resolve the overlap before starting Shearwater", n.CheckHostRanges(ctx))
	if os.Geteuid() == 0 {
		add("iptables tables", "filter/FORWARD and nat/POSTROUTING available",
			"check the host's iptables and permissions; keep Shearwater's chains dedicated to Shearwater", n.CheckFirewall(ctx))
	}
	return out
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("sw doctor", flag.ContinueOnError)
	var o opsOptions
	o.flags(fs)
	if err := parseOps(fs, args, &o); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	failed := false
	report := func(check hostCheck) {
		if check.err != nil {
			fmt.Printf("FAIL %s: %v\n  hint: %s\n", check.name, check.err, check.hint)
			failed = true
		} else {
			fmt.Printf("OK   %s: %s\n", check.name, check.detail)
		}
	}
	if o.Runtime == "containerd" && os.Geteuid() != 0 {
		fmt.Println("WARN root: run sw doctor as root for socket, firewall and server checks")
	}
	for _, c := range hostChecks(ctx, o) {
		report(c)
	}
	if o.Runtime == "containerd" {
		report(hostCheck{"node ownership", o.DataDir, "use the owning instance's flags from the error above", checkNodeOwner(o)})
	}
	apiCtx, apiCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer apiCancel()
	if err := apiReady(apiCtx, o); err != nil {
		// A stopped server can be perfectly healthy on disk. Treat
		// absence as a warning so doctor is useful before the first up.
		var status *operationError
		if errors.As(err, &status) && status.code == "NOT_RUNNING" {
			fmt.Printf("WARN server: %s\n", err)
		} else {
			report(hostCheck{"server", "", "check --listen, --config-dir and the server log", err})
		}
	} else {
		fmt.Printf("OK   server: authenticated HTTPS at %s\n", o.Listen)
		if o.Runtime == "containerd" {
			client, err := localAPI(o.ConfigDir, o.Listen)
			var nodes struct{ Items []apis.Node }
			if err == nil {
				err = client.get(apiCtx, "/api/v1/nodes", &nodes)
			}
			if err == nil && !slices.ContainsFunc(nodes.Items, func(n apis.Node) bool {
				return n.Metadata.Name == o.NodeName && slices.ContainsFunc(n.Status.Conditions, func(c apis.NodeCondition) bool {
					return c.Type == "Ready" && c.Status == "True"
				})
			}) {
				err = fmt.Errorf("node %s is not Ready yet", o.NodeName)
			}
			report(hostCheck{"node", o.NodeName + " Ready", "check the server log and containerd", err})
		}
	}
	fmt.Printf("INFO paths: data=%s config=%s run=%s logs=%s\n", o.DataDir, o.ConfigDir, o.RunDir, o.LogDir)
	fmt.Printf("INFO kubeconfig: %s (contains an admin token; keep it private)\n", filepath.Join(o.ConfigDir, "kubeconfig"))
	fmt.Println("INFO policy: Stage 1 uses the local admin token; policy rules are not applied yet")
	logPath := filepath.Join(o.LogDir, "server.log")
	fmt.Printf("INFO server log: %s\n", logPath)
	for _, line := range recentErrors(logPath) {
		fmt.Printf("  %s\n", line)
	}
	if failed {
		return opsError("PRECHECK_FAILED", "never", "one or more checks failed", "fix the FAIL lines above and rerun sw doctor")
	}
	return nil
}

func recentErrors(path string) []string {
	// Diagnostics must not load an unbounded log or print control
	// characters supplied by a workload, filename, or external tool.
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	start := max(fi.Size()-8192, 0)
	b := make([]byte, fi.Size()-start)
	n, _ := f.ReadAt(b, start)
	lines := strings.Split(string(b[:n]), "\n")
	if start > 0 {
		lines = lines[1:]
	}
	var out []string
	for _, line := range lines {
		l := strings.ToLower(line)
		if strings.Contains(l, "error") || strings.Contains(l, "fail") || strings.Contains(l, "refuse") || strings.HasPrefix(l, "sw:") {
			runes := []rune(strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return -1
				}
				return r
			}, line))
			out = append(out, string(runes[:min(400, len(runes))]))
		}
	}
	return out[max(0, len(out)-5):]
}
