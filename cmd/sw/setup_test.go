package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clientTransferFixture(t *testing.T, dir string) clientTransfer {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" && (r.URL.Path != "/api/v1/namespaces" || r.Header.Get("Authorization") != "Bearer fixture-only") {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Write([]byte("ok"))
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(root, "ca.crt"), ca, 0600); err != nil {
		t.Fatal(err)
	}
	c := &agentClient{token: "fixture-only"}
	identity, err := connectionIdentity(c, root)
	if err != nil {
		t.Fatal(err)
	}
	return clientTransfer{Connection: clientConnection{Format: "shearwater.client/v1", Config: dir, Listen: strings.TrimPrefix(server.URL, "https://"), Identity: identity}, CA: ca, Token: []byte("fixture-only\n")}
}

func TestClientDelegationRepeatConflictAndDefaults(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("normal-user ownership boundary")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := userClientDir(home)
	transfer := clientTransferFixture(t, dir)
	call := func() error {
		b, _ := json.Marshal(transfer)
		return receiveClient([]string{"--client-dir=" + dir}, bytes.NewReader(b), io.Discard)
	}
	if err := call(); err != nil {
		t.Fatal(err)
	}
	if err := call(); err != nil {
		t.Fatal("same credentials must be reusable", err)
	}
	con, err := clientDefaults()
	if err != nil || con != transfer.Connection {
		t.Fatal(con, err)
	}
	for _, name := range []string{"ca.crt", "admin.token", "connection.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(name, err)
		}
	}
	transfer.Token = []byte("different-token")
	if err = call(); err == nil {
		t.Fatal("overwrote existing credentials")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "admin.token"))
	if string(b) != "fixture-only\n" {
		t.Fatal("token changed")
	}
}

func TestSavedClientIsTheDefaultOnlyWithoutExplicitFlags(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("normal-user ownership boundary")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	parse := func(args ...string) (string, string) {
		t.Helper()
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		config := fs.String("config-dir", defaultConfigDir, "")
		listen := fs.String("listen", defaultListen, "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		if err := savedClient(fs, config, listen); err != nil {
			t.Fatal(err)
		}
		return *config, *listen
	}
	if c, l := parse(); c != defaultConfigDir || l != defaultListen {
		t.Fatal("without a saved connection the server defaults must stay", c, l)
	}
	dir := userClientDir(home)
	transfer := clientTransferFixture(t, dir)
	b, _ := json.Marshal(transfer)
	if err := receiveClient([]string{"--client-dir=" + dir}, bytes.NewReader(b), io.Discard); err != nil {
		t.Fatal(err)
	}
	if c, l := parse(); c != dir || l != transfer.Connection.Listen {
		t.Fatal("a normal user must reach the saved connection without flags", c, l)
	}
	if c, l := parse("--listen=127.0.0.1:1"); c != defaultConfigDir || l != "127.0.0.1:1" {
		t.Fatal("an explicit flag must turn the saved connection off", c, l)
	}
	if c, _ := parse("--config-dir=/srv/other"); c != "/srv/other" {
		t.Fatal("an explicit config-dir must win", c)
	}
}

func TestClientDelegationRejectsSymlinkAndOversize(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("normal-user ownership boundary")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "client")
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, dir); err != nil {
		t.Fatal(err)
	}
	transfer := clientTransferFixture(t, dir)
	b, _ := json.Marshal(transfer)
	if err := receiveClient([]string{"--client-dir=" + dir}, bytes.NewReader(b), io.Discard); err == nil {
		t.Fatal("followed symlink")
	}
	if err := receiveClient([]string{"--client-dir=" + dir}, strings.NewReader(strings.Repeat("x", 131073)), io.Discard); err == nil {
		t.Fatal("unbounded import")
	}
	entries, _ := os.ReadDir(other)
	if len(entries) != 0 {
		t.Fatal("wrote through unsafe destination")
	}
}

func TestEmptySelectorValueStillRequiresLabelKey(t *testing.T) {
	if routeMatches(map[string]string{"tier": ""}, map[string]string{}) {
		t.Fatal("missing label was accepted")
	}
	if !routeMatches(map[string]string{"tier": ""}, map[string]string{"tier": ""}) {
		t.Fatal("valid empty label rejected")
	}
}

func TestSetupRequiresAuthenticationBeyondPublicHealth(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.Write([]byte("ok"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer real-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()
	c := &agentClient{client: server.Client(), url: server.URL, token: "wrong-token"}
	if err := checkSetupAPI(context.Background(), c); err == nil {
		t.Fatal("public health endpoint was treated as authentication")
	}
	c.token = "real-token"
	if err := checkSetupAPI(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}
