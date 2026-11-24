package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

func (a *Agent) readVolumeObject(key store.Key, obj apis.Object) error {
	rec, err := a.Store.Get(key)
	if err != nil {
		return fmt.Errorf("read %s: %w", key, err)
	}
	return json.Unmarshal(rec.Data, obj)
}

func (a *Agent) volumeNode() (*apis.Node, error) {
	n := &apis.Node{}
	err := a.readVolumeObject(store.Key{Resource: "nodes", Name: a.NodeName}, n)
	return n, err
}

func (a *Agent) pvPath(pv *apis.PersistentVolume) (string, error) {
	var source string
	switch {
	case pv.Spec.Local != nil:
		source = pv.Spec.Local.Path
		// The provisioner publishes the canonical Linux path. Each worker
		// maps its managed directory under its own --data-dir.
		if strings.HasPrefix(pv.Metadata.Name, "pvc-") && source == apis.LocalVolumeRoot+"/"+pv.Metadata.Name {
			source = filepath.Join(a.DataDir, "volumes", pv.Metadata.Name)
		}
	case pv.Spec.HostPath != nil:
		source = pv.Spec.HostPath.Path
	}
	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("persistentvolume %q has no absolute filesystem source", pv.Metadata.Name)
	}
	return filepath.Clean(source), nil
}

func (a *Agent) claimDirectory(namespace, name string) (string, error) {
	var c apis.PersistentVolumeClaim
	if err := a.readVolumeObject(store.Key{Resource: "persistentvolumeclaims", Namespace: namespace, Name: name}, &c); err != nil {
		return "", err
	}
	if c.Status.Phase != "Bound" || c.Spec.VolumeName == "" {
		return "", fmt.Errorf("persistentvolumeclaim %q is not bound", name)
	}
	var pv apis.PersistentVolume
	if err := a.readVolumeObject(store.Key{Resource: "persistentvolumes", Name: c.Spec.VolumeName}, &pv); err != nil {
		return "", err
	}
	if pv.Metadata.DeletionTimestamp != nil || pv.Status.Phase != "Bound" || !pv.Claims(&c) {
		return "", fmt.Errorf("persistentvolume %q is not bound to this claim", pv.Metadata.Name)
	}
	node, err := a.volumeNode()
	if err != nil {
		return "", err
	}
	if !pv.MatchesNode(node) {
		return "", fmt.Errorf("persistentvolume %q: volume node affinity conflict on %q", pv.Metadata.Name, a.NodeName)
	}
	dir, err := a.pvPath(&pv)
	if err != nil {
		return "", err
	}
	// The agent runs as root. Leave the permissions/ownership of existing
	// application data alone; use an explicit mode on first creation.
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if err = os.Mkdir(dir, 0o755); err == nil {
			return dir, os.Chmod(dir, 0o755)
		}
		if errors.Is(err, os.ErrExist) {
			info, err = os.Stat(dir)
		}
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("persistentvolume %q: %s is not a directory", pv.Metadata.Name, dir)
	}
	return dir, nil
}

func claimReadOnly(pod *apis.Pod, volumeName string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.Name == volumeName && v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ReadOnly
		}
	}
	return false
}

// A static PV may reach a managed directory via a symlink. Resolve the
// existing prefix even when the agent has not created the last component yet.
func volumeRealPath(dir string) (string, error) {
	for prefix := dir; ; prefix = filepath.Dir(prefix) {
		resolved, err := filepath.EvalSymlinks(prefix)
		if err == nil {
			suffix, err := filepath.Rel(prefix, dir)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, suffix), nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(prefix) == prefix {
			return "", err
		}
	}
}

func keepVolumeDirectory(candidates map[string]bool, root, dir string) {
	if dir == root || dir == string(filepath.Separator) || strings.HasPrefix(root, dir+string(filepath.Separator)) {
		clear(candidates)
	} else if rel, ok := strings.CutPrefix(dir, root+string(filepath.Separator)); ok {
		top, _, _ := strings.Cut(rel, string(filepath.Separator))
		delete(candidates, top)
	}
}

// collectVolumes enumerates disk before reading the API: a volume first
// mounted during the subsequent list cannot be mistaken for an old orphan.
// Read or decode errors abort the entire pass before any directory is removed.
func (a *Agent) collectVolumes() error {
	root := filepath.Join(a.DataDir, "volumes")
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !filepath.IsAbs(root) || !info.IsDir() {
		return fmt.Errorf("invalid volume root %q", root)
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	candidates := map[string]bool{}
	for _, d := range dirs {
		if d.IsDir() && strings.HasPrefix(d.Name(), "pvc-") {
			candidates[d.Name()] = true
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	n, err := a.volumeNode()
	if err != nil {
		return err
	}
	records, _, err := a.Store.List("", "persistentvolumes", "")
	if err != nil {
		return err
	}
	for _, rec := range records {
		var pv apis.PersistentVolume
		if err := json.Unmarshal(rec.Data, &pv); err != nil {
			return err
		}
		if !pv.MatchesNode(n) {
			continue
		}
		dir, err := a.pvPath(&pv)
		if err != nil {
			return err
		}
		realDir, err := volumeRealPath(dir)
		if err != nil {
			return err
		}
		// A static PV may name a subdirectory of a managed directory, or
		// expose the entire root. Protect its data irrespective of PV name.
		keepVolumeDirectory(candidates, root, dir)
		keepVolumeDirectory(candidates, realRoot, realDir)
	}
	if len(candidates) == 0 {
		return nil
	}
	// Pin the directory we enumerated across recursive removal. A rename of
	// the data root during resync must never redirect GC into its replacement.
	handle, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer handle.Close()
	current, err := handle.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return fmt.Errorf("volume root %q changed during collection", root)
	}
	for name := range candidates {
		if err := handle.RemoveAll(name); err != nil {
			return err
		}
	}
	return nil
}
