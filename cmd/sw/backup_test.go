package main

import (
	"bytes"
	"os"
	"path/filepath"
	"shearwater/internal/pki"
	"shearwater/internal/store"
	"strings"
	"testing"
)

func TestBackupRestoreOnlineAndCorruption(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	config := filepath.Join(base, "config")
	os.Mkdir(data, 0700)
	st, err := store.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = pki.Ensure(config); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(config, "admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(base, "backup")
	var out bytes.Buffer
	args := []string{"--data-dir=" + data, "--config-dir=" + config, "--run-dir=" + filepath.Join(base, "run"), "--log-dir=" + filepath.Join(base, "log"), "--runtime=none", "--output=" + dest}
	if err = runBackup(args, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "control-plane-only") {
		t.Fatal(out.String())
	}
	target := filepath.Join(base, "restored")
	if err = runRestore([]string{"--from=" + dest, "--to=" + target}, &out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(target, "config/admin.token"))
	if !bytes.Equal(got, original) {
		t.Fatal("credentials changed")
	}
	if err = verifyDB(filepath.Join(target, "data/state.db")); err != nil {
		t.Fatal(err)
	}
	if err = runRestore([]string{"--from=" + dest, "--to=" + target}, &out); err == nil {
		t.Fatal("overwrote an existing restore")
	}
	if err = runBackup(args, &out); err == nil {
		t.Fatal("overwrote a backup")
	}
	os.WriteFile(filepath.Join(dest, "config/admin.token"), []byte("changed"), 0600)
	if err = runRestore([]string{"--from=" + dest, "--verify-only"}, &out); err == nil {
		t.Fatal("accepted corrupted backup")
	}
}
func TestBackupRefusesRunningVolumeSnapshot(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	config := filepath.Join(root, "config")
	os.Mkdir(data, 0700)
	pki.Ensure(config)
	lock, err := fileLock(data, "database.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	var out bytes.Buffer
	err = runBackup([]string{"--data-dir=" + data, "--config-dir=" + config, "--output=" + filepath.Join(root, "backup"), "--include-volumes", "--runtime=none"}, &out)
	if err == nil || !strings.Contains(err.Error(), "stop the server") {
		t.Fatal(err)
	}
}
func TestSystemdUnitEscapingAndForeground(t *testing.T) {
	o := opsOptions{DataDir: "/var/lib/a%b$c", Runtime: "none"}
	unit, err := serviceUnit("/opt/shearwater/sw", o)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"Type=exec", "\"supervise\"", "Restart=on-failure", "a%%b$$c", "KillMode=process", "UMask=0022"} {
		if !strings.Contains(unit, v) {
			t.Fatalf("missing %s", v)
		}
	}
	if _, err = serviceUnit("/opt/sw\nExecStart=/bad", o); err == nil {
		t.Fatal("accepted unit injection")
	}
}

func TestBackupRefusesCorruptServerReceipt(t *testing.T) {
	root := t.TempDir()
	data, config, run := filepath.Join(root, "data"), filepath.Join(root, "config"), filepath.Join(root, "run")
	for _, dir := range []string{data, run} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pki.Ensure(config); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "server.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	dest := filepath.Join(root, "backup")
	err := runBackup([]string{"--data-dir=" + data, "--config-dir=" + config, "--run-dir=" + run, "--output=" + dest, "--include-volumes", "--runtime=none"}, &out)
	if err == nil || !strings.Contains(err.Error(), "cannot verify that the server stopped") {
		t.Fatalf("corrupt process receipt must refuse a volume backup: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("created backup before checking the server: %v", err)
	}
}

func TestBackupRejectsSymlinkInSourceParent(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "private"), []byte("must not be copied"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	var entries []backupEntry
	budget := int64(1024)
	err := backupTree(filepath.Join(root, "link/private"), filepath.Join(root, "backup"), "volumes/test", &entries, &budget)
	if err == nil || !strings.Contains(err.Error(), "symlink") || len(entries) != 0 {
		t.Fatal("followed a symlink in a source parent", err)
	}
}
