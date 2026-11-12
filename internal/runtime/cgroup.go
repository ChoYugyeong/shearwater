package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// removeEmptyPodCgroups is called only for a validated, owned Pod after its
// runtime tasks have been removed. Normally runc already removed every leaf.
// A WSL distribution restart can kill runc/shim without rebooting the shared
// kernel, leaving an empty old-attempt cgroup that containerd no longer knows.
// Remove such immediate children before the Pod directory itself.
//
// This is deliberately NOT recursive cleanup. Rmdir cannot unlink control
// files or symlinks; the cgroup filesystem refuses a populated group or one
// with children. We neither kill nor migrate processes to make removal work.
// Thus a concurrent/live owner makes cleanup fail safely and can be retried.
func removeEmptyPodCgroups(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	remove := func(dir string) error {
		if err := syscall.Rmdir(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &os.PathError{Op: "rmdir empty cgroup", Path: dir, Err: err}
		}
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := remove(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
	}
	return remove(path)
}
