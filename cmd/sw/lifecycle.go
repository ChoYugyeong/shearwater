package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"shearwater/internal/apis"
	"shearwater/internal/pki"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

const (
	stateFile = "server.json"
	ownerFile = "node-owner.json"
)

var errNoNodeOwner = errors.New("no managed node")

type serverState struct {
	PID       int        `json:"pid"`
	StartTick string     `json:"startTick"`
	Instance  string     `json:"instance"`
	Options   opsOptions `json:"options"`
}

type nodeOwnership struct {
	Options opsOptions `json:"options"`
	Managed bool       `json:"managed"`
}

// The PID file is a receipt, not authority to signal a number. The process
// can die without clearing it, and Linux may give that number to another
// program. Check the start tick and the random child argument through a pidfd.
func (s serverState) openProcess() (int, error) {
	if s.PID < 2 || len(s.Instance) != 32 || s.StartTick == "" {
		return -1, fmt.Errorf("invalid managed server record")
	}
	fd, err := unix.PidfdOpen(s.PID, 0)
	if errors.Is(err, unix.ESRCH) {
		return -1, nil
	}
	if err != nil {
		return -1, fmt.Errorf("open safe process handle for PID %d: %w", s.PID, err)
	}
	// pidfd_open takes a PID in the caller's namespace, while /proc may
	// be mounted from a different namespace. Verify the descriptor itself
	// resolves to the recorded PID before looking up that PID in /proc.
	target, err := pidfdProcPID(fd)
	if err != nil || target != s.PID {
		unix.Close(fd)
		if target == -1 && err == nil {
			return -1, nil // the process exited before inspection
		}
		return -1, fmt.Errorf("cannot verify PID %d against this /proc mount (descriptor PID %d, %v)", s.PID, target, err)
	}
	start, err := processStart(s.PID)
	if err != nil || start != s.StartTick {
		unix.Close(fd)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return -1, err
		}
		return -1, nil
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", s.PID))
	if err != nil || !bytes.Contains(cmdline, []byte("\x00server\x00--managed="+s.Instance+"\x00")) {
		unix.Close(fd)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return -1, err
		}
		return -1, nil
	}
	return fd, nil
}

func pidfdProcPID(fd int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if err != nil {
		return 0, err
	}
	for _, row := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(row, "Pid:"); ok {
			return strconv.Atoi(strings.TrimSpace(value))
		}
	}
	return 0, errors.New("fdinfo does not identify the process")
}

func processStart(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	// The executable name is parenthesized and may contain spaces or ')'.
	// After its final ')' the fields begin at 3; starttime is field 22.
	j := bytes.LastIndexByte(data, ')')
	fields := strings.Fields(string(data[j+1:]))
	if j < 0 || len(fields) < 20 {
		return "", fmt.Errorf("invalid process stat for PID %d", pid)
	}
	return fields[19], nil
}

func readState(dir string) (serverState, error) {
	var st serverState
	err := readPrivateJSON(filepath.Join(dir, stateFile), &st)
	return st, err
}

func writeState(o opsOptions, instance string) error {
	pid := os.Getpid()
	visible, err := os.Readlink("/proc/self")
	if err != nil || visible != strconv.Itoa(pid) {
		return errors.New("managed servers need /proc mounted for their own PID namespace")
	}
	start, err := processStart(pid)
	if err != nil {
		return err
	}
	return atomicJSON(filepath.Join(o.RunDir, stateFile), serverState{pid, start, instance, o})
}

func atomicJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicPrivateFile(path, append(b, '\n'))
}

// Keep an existing identical file unchanged. Publish a complete replacement
// by renaming a private temporary file in the same directory.
func atomicPrivateFile(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 {
		if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, data) {
			return nil
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".sw-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func checkPrivateDir(dir string, allowMissing bool) error {
	// Validate the existing ancestors before creating anything. A
	// root-owned final directory inside somebody else's writable parent
	// could still be swapped for a symlink. A root-owned sticky /tmp is
	// fine: other users cannot replace the child we own.
	var lineage []string
	for path := dir; ; path = filepath.Dir(path) {
		lineage = append(lineage, path)
		if filepath.Dir(path) == path {
			break
		}
	}
	uid := uint32(os.Geteuid())
	for i := len(lineage) - 1; i >= 0; i-- {
		path := lineage[i]
		info, err := os.Lstat(path)
		if allowMissing && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		owner := info.Sys().(*syscall.Stat_t).Uid
		if !info.IsDir() || (owner != uid && (owner != 0 || i == 0)) ||
			(info.Mode().Perm()&0o022 != 0 && !(i != 0 && owner == 0 && info.Mode()&os.ModeSticky != 0)) {
			return fmt.Errorf("unsafe instance path %s: use owned directories without symlinks or group/other write access", path)
		}
	}
	return nil
}

func privateDir(dir string, create bool) error {
	if err := checkPrivateDir(dir, create); err != nil || !create {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return checkPrivateDir(dir, false)
}

// Preserve the existing log location on hosts where it is safe. On Ubuntu
// WSL, /var/log can be writable by the syslog group, so the default belongs
// under Shearwater's private data directory on that host.
func defaultLogPath(preferred, dataDir string) string {
	if checkPrivateDir(preferred, true) == nil {
		return preferred
	}
	return filepath.Join(dataDir, "log")
}

func readPrivateJSON(path string, dst any) error {
	f, err := openPrivate(path, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return err
	}
	if len(b) > 65536 {
		return fmt.Errorf("managed state file is too large: %s", path)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("read managed state %s: %w", path, err)
	}
	return nil
}

func openPrivate(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("%s must be a private regular file owned by this user", path)
	}
	return os.NewFile(uintptr(fd), path), nil
}

func fileLock(dir, name string) (*os.File, error) {
	f, err := openPrivate(filepath.Join(dir, name), unix.O_CREAT|unix.O_RDWR)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, opsError("CONFLICT", "safe", fmt.Sprintf("%s is locked (%v)", filepath.Join(dir, name), err), "finish the other Shearwater operation before retrying")
	}
	return f, nil
}

func lockNode() (*os.File, error) {
	if err := privateDir(defaultRunDir, true); err != nil {
		return nil, err
	}
	return fileLock(defaultRunDir, "node.lock")
}

// A stopped managed node still owns the shared bridge, addresses and tasks.
// A second instance with a different DB must not adopt or clean them up.
func readNodeOwner() (nodeOwnership, error) {
	var owner nodeOwnership
	err := readPrivateJSON(filepath.Join(defaultRunDir, ownerFile), &owner)
	return owner, err
}

func checkNodeOwner(o opsOptions) error {
	owner, err := readNodeOwner()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner.Options != o {
		return opsError("CONFLICT", "never", fmt.Sprintf("the node belongs to %s (run directory %s)", owner.Options.DataDir, owner.Options.RunDir),
			"use the owning instance's flags; sw down --all releases its runtime and network")
	}
	return nil
}

// Prevent one instance from replacing the kubeconfig printed by another.
// Ports and volatile directories may change when the same DB is restored.
func lockConfig(o opsOptions) (*os.File, error) {
	if err := privateDir(o.ConfigDir, true); err != nil {
		return nil, err
	}
	lock, err := fileLock(o.ConfigDir, "config.lock")
	if err != nil {
		return nil, err
	}
	var owner struct{ DataDir string }
	path := filepath.Join(o.ConfigDir, "config-owner.json")
	if err := readPrivateJSON(path, &owner); errors.Is(err, os.ErrNotExist) {
		owner.DataDir = o.DataDir
		if err := atomicJSON(path, owner); err != nil {
			lock.Close()
			return nil, err
		}
	} else if err != nil {
		lock.Close()
		return nil, err
	} else if owner.DataDir != o.DataDir {
		lock.Close()
		return nil, opsError("CONFLICT", "never", fmt.Sprintf("credentials in %s belong to the database at %s", o.ConfigDir, owner.DataDir),
			"choose a different --config-dir for a different database")
	}
	return lock, nil
}

func runUp(args []string) error {
	fs := flag.NewFlagSet("sw up", flag.ContinueOnError)
	var o opsOptions
	o.flags(fs)
	if err := parseOps(fs, args, &o); err != nil {
		return err
	}
	if o.Runtime == "containerd" && os.Geteuid() != 0 {
		return needRoot("start the node")
	}
	if err := privateDir(o.RunDir, true); err != nil {
		return err
	}
	lock, err := fileLock(o.RunDir, "control.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if st, err := readState(o.RunDir); err == nil {
		fd, err := st.openProcess()
		if err != nil {
			return err
		}
		if fd >= 0 {
			unix.Close(fd)
			if st.Options != o {
				return opsError("CONFLICT", "never", "another managed server is using "+o.RunDir, "reuse the flags with which it was started")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := apiReady(ctx, o); err != nil {
				return err
			}
			return printKubeconfig(o, "already running")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := preflight(o); err != nil {
		return err
	}
	for _, dir := range []string{o.ConfigDir, o.DataDir, o.LogDir} {
		if err := privateDir(dir, true); err != nil {
			return err
		}
	}
	logPath := filepath.Join(o.LogDir, "server.log")
	logFile, err := openPrivate(logPath, unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND)
	if err != nil {
		return err
	}
	defer logFile.Close()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	var random [16]byte
	rand.Read(random[:])
	id := hex.EncodeToString(random[:])
	cmd := exec.Command(exe, "server", "--managed="+id, "--listen="+o.Listen, "--data-dir="+o.DataDir,
		"--config-dir="+o.ConfigDir, "--run-dir="+o.RunDir, "--log-dir="+o.LogDir,
		"--containerd-socket="+o.Socket, "--node-name="+o.NodeName, "--runtime="+o.Runtime)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if st, err := readState(o.RunDir); err == nil && st.PID == cmd.Process.Pid && st.Instance == id {
			if err := apiReady(ctx, o); err == nil {
				return printKubeconfig(o, "running")
			}
		}
		select {
		case err := <-done:
			return opsError("RUNTIME_ERROR", "safe", fmt.Sprintf("server stopped during startup: %v", err), "read "+logPath+" and run sw doctor with the same flags")
		case <-ctx.Done():
			cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				cmd.Process.Kill()
				<-done
			}
			return opsError("TIMEOUT", "safe", "server did not become ready", "read "+logPath)
		case <-tick.C:
		}
	}
}

func apiReady(ctx context.Context, o opsOptions) error {
	client, err := localAPI(o.ConfigDir, o.Listen)
	if err != nil {
		return err
	}
	if err := client.get(ctx, "/readyz", nil); err != nil {
		return err
	}
	return client.get(ctx, "/api", nil) // also checks that the generated credentials work
}

func printKubeconfig(o opsOptions, state string) error {
	mat, err := pki.Load(o.ConfigDir)
	if err != nil {
		return err
	}
	path := filepath.Join(o.ConfigDir, "kubeconfig")
	if err := writeKubeconfig(o, mat); err != nil {
		return err
	}
	fmt.Printf("sw server %s at https://%s\nkubeconfig: %s\nkubectl --kubeconfig %s get nodes\n", state, o.Listen, path, shellQuote(path))
	return nil
}

func writeKubeconfig(o opsOptions, mat *pki.Material) error {
	return atomicPrivateFile(filepath.Join(o.ConfigDir, "kubeconfig"), []byte(pki.Kubeconfig(mat, "https://"+o.Listen)))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func runDown(args []string) error {
	fs := flag.NewFlagSet("sw down", flag.ContinueOnError)
	var o opsOptions
	o.flags(fs)
	all := fs.Bool("all", false, "also stop Pods and detach the owned network; keep persistent data")
	if err := parseOps(fs, args, &o); err != nil {
		return err
	}
	if o.Runtime == "containerd" && os.Geteuid() != 0 {
		return needRoot("stop the node")
	}
	if err := privateDir(o.RunDir, false); errors.Is(err, os.ErrNotExist) && !*all {
		fmt.Println("sw server already stopped")
		return nil
	} else if err != nil {
		return err
	}
	lock, err := fileLock(o.RunDir, "control.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	active := false
	st, err := readState(o.RunDir)
	if err == nil {
		if st.Options != o {
			return opsError("CONFLICT", "never", "the server record does not match these flags", "use the same instance flags as sw up")
		}
		fd, err := st.openProcess()
		if err != nil {
			return err
		}
		if fd >= 0 {
			active = true
			defer unix.Close(fd)
			if err := stopServer(fd); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !active {
		// A foreground server or a child that has not published its
		// receipt yet must not be mistaken for a stopped managed one.
		probe, err := fileLock(o.RunDir, "instance.lock")
		if err != nil {
			return err
		}
		defer probe.Close()
		if !*all || o.Runtime == "none" {
			ln, err := net.Listen("tcp", o.Listen)
			if err != nil {
				return opsError("CONFLICT", "never", "the address is in use by a process that sw down cannot identify", "stop the foreground server in its own terminal or check --listen")
			}
			ln.Close()
		}
		// With --all, the node lock and ownership record authorize
		// teardown of the stopped node independently of its old API port.
	}
	if !*all || o.Runtime == "none" {
		fmt.Println("sw server stopped; workloads and data were kept")
		return nil
	}
	if err := stopNode(o); errors.Is(err, errNoNodeOwner) {
		fmt.Println("no managed node to tear down; use sw up with its original flags to take ownership")
		return nil
	} else if err != nil {
		return err
	}
	fmt.Println("sw server and Pods stopped; Shearwater network removed; data and credentials kept")
	return nil
}

func stopServer(fd int) error {
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	// A readable pidfd means the exact process has exited, even if a
	// supervisor has not reaped it yet. Never signal a numeric PID again.
	deadline := time.Now().Add(17 * time.Second)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for time.Now().Before(deadline) {
		n, err := unix.Poll(fds, int(time.Until(deadline).Milliseconds()))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0 {
			return nil
		}
		if n > 0 {
			return fmt.Errorf("unexpected process handle event %#x", fds[0].Revents)
		}
	}
	return opsError("TIMEOUT", "safe", "server has not exited after SIGTERM", "check the server log; no Pods or host network were removed")
}

func knownPodUIDs(st *store.Store) (map[string]bool, error) {
	ips, err := st.IPs("pod")
	if err != nil {
		return nil, err
	}
	pods, _, err := st.List("", "pods", "")
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(pods)+len(ips))
	for uid := range ips {
		known[uid] = true
	}
	for _, pod := range pods {
		known[pod.UID] = true
	}
	return known, nil
}

// The caller holds the host node lock. Inspect all our runtime records and
// network handles before touching anything, including orphan reservations.
func ownedRuntime(ctx context.Context, st *store.Store, rt *runtime.Containerd, netw *runtime.Net) ([]runtime.Container, map[string]bool, error) {
	known, err := knownPodUIDs(st)
	if err != nil {
		return nil, nil, err
	}
	cs, err := rt.Containers(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	ns, err := netw.Pods()
	if err != nil {
		return nil, nil, err
	}
	for _, c := range cs {
		if !known[c.PodUID] {
			return nil, nil, opsError("CONFLICT", "never", "container "+c.ID+" is not owned by this database", "use the instance that created it")
		}
	}
	for _, uid := range ns {
		if !known[uid] {
			return nil, nil, opsError("CONFLICT", "never", "network namespace "+uid+" is not owned by this database", "use the instance that created it")
		}
	}
	return cs, known, nil
}

// A foreground test server that has cleaned up all Pods and Services can
// release the node for the next instance. If anything is still live or we
// cannot prove it is gone, keep the receipt. Managed servers release only
// through down --all, which also removes the host network.
func releaseIdleNode(st *store.Store, rt *runtime.Containerd, n *runtime.Net) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cs, err := rt.Containers(ctx, "")
	if err != nil || len(cs) != 0 {
		return
	}
	ns, err := n.Pods()
	if err != nil || len(ns) != 0 {
		return
	}
	for _, kind := range []string{"pod", "service"} {
		ips, err := st.IPs(kind)
		if err != nil || len(ips) != 0 {
			return
		}
	}
	if err := os.Remove(filepath.Join(defaultRunDir, ownerFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "sw: could not release the idle node:", err)
	}
}

func stopNode(o opsOptions) error {
	lock, err := lockNode()
	if err != nil {
		return err
	}
	defer lock.Close()
	owner, err := readNodeOwner()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errNoNodeOwner
		}
		return err
	}
	if owner.Options != o {
		return checkNodeOwner(o)
	}
	if err := privateDir(o.DataDir, false); err != nil {
		return err
	}
	path := filepath.Join(o.DataDir, "state.db")
	if info, err := os.Lstat(path); err != nil {
		return fmt.Errorf("cannot verify which Pods own this node: %w", err)
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular SQLite file", path)
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rt := runtime.NewContainerd(o.Socket)
	defer rt.Close()
	n := runtime.NewNet(o.RunDir, apis.PodCIDR, apis.ServiceCIDR)
	cs, uids, err := ownedRuntime(ctx, st, rt, n)
	if err != nil {
		return err
	}
	for _, c := range runtime.RemovalOrder(cs) {
		if err := rt.Remove(ctx, c.ID); err != nil {
			return err
		}
	}
	for uid := range uids {
		if err := n.Teardown(uid); err != nil {
			return err
		}
		if err := rt.PodGone(ctx, uid); err != nil {
			return err
		}
	}
	if err := n.TeardownHost(ctx); err != nil {
		return err
	}
	// Only the ephemeral ownership receipt is released. SQLite, IP
	// reservations, volumes, images, and logs survive a cold start.
	return os.Remove(filepath.Join(defaultRunDir, ownerFile))
}

func needRoot(action string) error {
	return opsError("FORBIDDEN", "never", "root is required to "+action, "run inside WSL as root, or use --runtime=none for an API-only instance")
}

func preflight(o opsOptions) error {
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return opsError("CONFLICT", "safe", fmt.Sprintf("cannot listen on %s: %v", o.Listen, err), "stop the process using this port or pass a different --listen")
	}
	ln.Close()
	if o.Runtime == "containerd" {
		if err := checkNodeOwner(o); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	for _, check := range hostChecks(ctx, o) {
		if check.err != nil {
			return opsError("PRECHECK_FAILED", "never", check.name+": "+check.err.Error(), check.hint)
		}
	}
	return nil
}
