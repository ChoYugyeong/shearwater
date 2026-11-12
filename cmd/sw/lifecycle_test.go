package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestManagedReceiptRejectsUnrelatedProcess(t *testing.T) {
	// Even a live PID with the right start time is not enough: the target
	// must be the exact server launched for this receipt.
	pid := os.Getpid()
	start, err := processStart(pid)
	if err != nil {
		t.Fatal(err)
	}
	st := serverState{PID: pid, StartTick: start, Instance: "00000000000000000000000000000000"}
	fd, err := st.openProcess()
	if errors.Is(err, unix.ENOSYS) {
		t.Skip("kernel has no pidfd")
	}
	if err != nil {
		t.Fatal(err)
	}
	if fd >= 0 {
		unix.Close(fd)
		t.Fatal("unrelated test process matched managed server receipt")
	}
}

func TestManagedDirectoryRefusesRedirectedParent(t *testing.T) {
	dir := t.TempDir()
	dst := t.TempDir()
	link := filepath.Join(dir, "redirect")
	if err := os.Symlink(dst, link); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(filepath.Join(link, "run"), true); err == nil {
		t.Fatal("created an instance directory through a symlink")
	}
	if _, err := os.Stat(filepath.Join(dst, "run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected filesystem change through a symlink: %v", err)
	}
}

func TestDefaultLogPathFallsBackWhenParentIsGroupWritable(t *testing.T) {
	parent := t.TempDir()
	preferred := filepath.Join(parent, "shearwater")
	data := t.TempDir()
	if got := defaultLogPath(preferred, data); got != preferred {
		t.Fatalf("default log path with a private parent = %q, want %q", got, preferred)
	}
	if err := os.Chmod(parent, 0o775); err != nil {
		t.Fatal(err)
	}
	if got, want := defaultLogPath(preferred, data), filepath.Join(data, "log"); got != want {
		t.Fatalf("default log path with a group-writable parent = %q, want %q", got, want)
	}
	if _, err := os.Stat(preferred); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default selection created the log directory: %v", err)
	}
}

func TestConfigRemainsBoundToOriginalDatabaseAfterStop(t *testing.T) {
	first := opsOptions{ConfigDir: t.TempDir(), DataDir: t.TempDir()}
	lock, err := lockConfig(first)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close() // the first server stopped, but its kubeconfig is still in use
	second := first
	second.DataDir = t.TempDir()
	if lock, err := lockConfig(second); err == nil {
		lock.Close()
		t.Fatal("a second database claimed the original server's config directory")
	}
}

func TestDownDoesNotStopUnknownListener(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	err = runDown([]string{"--runtime=none", "--listen=" + ln.Addr().String(), "--run-dir=" + t.TempDir()})
	var result *operationError
	if !errors.As(err, &result) || result.code != "CONFLICT" {
		t.Fatalf("down should refuse the unverified listener, got %v", err)
	}
	conn, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}
