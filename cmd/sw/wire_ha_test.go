package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"shearwater/internal/ha"
	"shearwater/internal/pki"
	"shearwater/internal/store"
)

func certIPs(t *testing.T, certPEM []byte) []string {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM certificate")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	return out
}

func TestHAWiringEnableAndJoin(t *testing.T) {
	cfgDir, dataDir := t.TempDir(), t.TempDir()
	if _, err := pki.Ensure(cfgDir); err != nil {
		t.Fatal(err)
	}
	// runServer gives the leader's certificate its advertised address.
	if err := pki.EnsureServerAddresses(cfgDir, []net.IP{net.ParseIP("192.168.77.10")}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.InitCredentials(); err != nil {
		t.Fatal(err)
	}
	reloaded := 0
	w := &wiring{Store: st, ConfigDir: cfgDir, DataDir: dataDir, NodeName: "cp1"}
	h := &haWiring{w: w, self: "cp1", advertise: netip.MustParseAddr("192.168.77.10"), port: "6443", logf: t.Logf,
		reloadTLS: func() error { reloaded++; return nil }}
	if _, err := h.Join(ha.RoleControlPlane, "cp2", "192.168.77.11"); err == nil {
		t.Fatal("joined before high availability was enabled")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.Enable(); err != nil {
		t.Fatal(err)
	}
	cfg, err := ha.Load(cfgDir)
	if err != nil || cfg.Role != ha.RoleLeader || cfg.Epoch != 1 || len(cfg.Members) != 1 {
		t.Fatalf("ha.json after Enable: %+v %v", cfg, err)
	}
	if err := h.Enable(); err != nil {
		t.Fatal("Enable is not idempotent:", err)
	}
	// The leader can later run its node as a worker of the other control plane.
	if _, err := os.Stat(filepath.Join(cfgDir, "standby", nodeConfigFile)); err != nil {
		t.Fatal("no worker credentials for a later step-down:", err)
	}
	if creds, err := st.Credentials("node"); err != nil || len(creds) != 1 || creds[0].Name != "cp1" {
		t.Fatalf("node credential: %+v %v", creds, err)
	}
	// Writes are now logged for a standby.
	if err := st.Write(false, func(tx *store.Tx) error {
		_, err := tx.Create(store.Put{Key: store.Key{Resource: "namespaces", Name: "x"}, UID: "x",
			Encode: func(int64) ([]byte, error) { return []byte(`{}`), nil }})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if seq, _ := st.ReplicationState(); seq == 0 {
		t.Fatal("no replication log after Enable")
	}

	b, err := h.Join(ha.RoleControlPlane, "cp2", "192.168.77.11")
	if err != nil {
		t.Fatal(err)
	}
	if b.Config.Self != "cp2" || b.Config.Role != ha.RoleStandby || len(b.Config.Members) != 2 || b.Config.Token != cfg.Token {
		t.Fatalf("control plane bundle config: %+v", b.Config)
	}
	admin, _ := os.ReadFile(filepath.Join(cfgDir, "admin.token"))
	key, _ := os.ReadFile(filepath.Join(cfgDir, "ca.key"))
	if b.CAKey != string(key) || b.AdminToken != strings.TrimSpace(string(admin)) || b.CACert == "" {
		t.Fatal("control plane bundle lacks the CA key, the administrator token or the CA")
	}
	srv, _ := os.ReadFile(filepath.Join(cfgDir, "server.crt"))
	if ips := certIPs(t, srv); !slices.Contains(ips, "192.168.77.11") || !slices.Contains(ips, "192.168.77.10") || reloaded != 1 {
		t.Fatalf("the leader's certificate lacks the standby's address (reloaded %d): %v", reloaded, certIPs(t, srv))
	}
	if got := h.ControlPlanes(); !slices.Equal(got, []string{"https://192.168.77.10:6443", "https://192.168.77.11:6443"}) {
		t.Fatal(got)
	}
	if _, err := h.Join(ha.RoleControlPlane, "cp3", "192.168.77.13"); err == nil {
		t.Fatal("a third control plane joined")
	}
	if _, err := h.Join(ha.RoleControlPlane, "cp2", "192.168.77.11"); err != nil {
		t.Fatal("the same standby cannot join again:", err)
	}

	wb, err := h.Join(ha.RoleWitness, "w1", "192.168.77.12")
	if err != nil {
		t.Fatal(err)
	}
	if wb.Listen != "192.168.77.12:6445" || wb.CAKey != "" || wb.AdminToken != "" || wb.Config.Token != cfg.Token {
		t.Fatalf("witness bundle: listen %q, secrets beyond the HA token: %v", wb.Listen, wb.CAKey != "" || wb.AdminToken != "")
	}
	if !slices.Contains(certIPs(t, []byte(wb.ServingCert)), "192.168.77.12") {
		t.Fatal("the witness certificate lacks its address")
	}
	cfg, err = ha.Load(cfgDir)
	if err != nil || cfg.Witness != "https://192.168.77.12:6445" || len(cfg.Members) != 2 {
		t.Fatalf("ha.json after joins: %+v %v", cfg, err)
	}
	if _, err := h.Join("worker", "w2", "192.168.77.14"); err == nil {
		t.Fatal("a worker role got an HA bundle")
	}
}
