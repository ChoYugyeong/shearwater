package main

import (
	"path/filepath"
	"testing"
)

func TestUserKubeconfigNamesTheAdvertisedAddress(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "etc")
	if got := userServer(dir, "https://127.0.0.1:6443"); got != "https://127.0.0.1:6443" {
		t.Fatalf("a single server keeps its local address, got %s", got)
	}
	if err := saveAdvertise(dir, "192.168.1.10"); err != nil {
		t.Fatal(err)
	}
	if got := userServer(dir, "https://127.0.0.1:16443"); got != "https://192.168.1.10:16443" {
		t.Fatalf("a control plane others join must hand users its advertised address, got %s", got)
	}
}
