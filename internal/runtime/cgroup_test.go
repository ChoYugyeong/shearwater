package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveEmptyPodCgroups(t *testing.T) {
	parent := t.TempDir()
	pod := filepath.Join(parent, "owned-pod")
	for _, name := range []string{"web", "web_1", "web_2"} {
		if err := os.MkdirAll(filepath.Join(pod, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(parent, "other-pod")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	for range 2 { // Cleanup is also idempotent after the whole Pod disappeared.
		if err := removeEmptyPodCgroups(pod); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(pod); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned Pod still exists: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("unrelated sibling was touched: %v", err)
	}
}

func TestRemoveEmptyPodCgroupsDoesNotForceRemoval(t *testing.T) {
	// Ordinary directories model the rmdir boundary, not the cgroup kernel:
	// nonempty/nested entries and symlinks must never be force-removed. The
	// real cgroup filesystem additionally rejects groups with live processes.
	for _, kind := range []string{"file", "nested", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			pod := filepath.Join(parent, "owned-pod")
			leaf := filepath.Join(pod, "web_1")
			if err := os.MkdirAll(leaf, 0700); err != nil {
				t.Fatal(err)
			}
			protected := filepath.Join(leaf, "keep")
			switch kind {
			case "file":
				if err := os.WriteFile(protected, []byte("do not unlink"), 0600); err != nil {
					t.Fatal(err)
				}
			case "nested":
				if err := os.Mkdir(protected, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				protected = filepath.Join(pod, "foreign-link")
				if err := os.Symlink(parent, protected); err != nil {
					t.Fatal(err)
				}
			}
			if err := removeEmptyPodCgroups(pod); err == nil {
				t.Fatal("unexpectedly accepted nonempty Pod directory")
			}
			if _, err := os.Lstat(protected); err != nil {
				t.Fatalf("protected entry was removed: %v", err)
			}
		})
	}
}

func TestPodGoneRejectsInvalidUID(t *testing.T) {
	for _, uid := range []string{"", ".", "..", "../other-pod", "/other-pod"} {
		if err := (&Containerd{}).PodGone(context.Background(), uid); err == nil {
			t.Fatalf("accepted invalid Pod UID %q", uid)
		}
	}
}
