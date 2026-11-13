package node

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
	// Stage 1: the host's resolver configuration, copied once (design 7).
	if resolv := filepath.Join(dir, "resolv.conf"); !exists(resolv) {
		data, _ := os.ReadFile("/etc/resolv.conf")
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
