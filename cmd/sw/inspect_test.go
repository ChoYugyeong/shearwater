package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"shearwater/internal/api"
	"shearwater/internal/pki"
	"shearwater/internal/store"
)

func TestInspectReadsExistingAPIOverHTTPS(t *testing.T) {
	material, err := pki.Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A reader only needs the CA and token. Server keys and a kubeconfig
	// need not exist here, and the commands should leave this directory alone.
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "ca.crt"), material.CACert, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "admin.token"), []byte(material.Token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &api.Server{Store: st, Token: material.Token}
	if err := s.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	body := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"waiting"},"spec":{"containers":[{"name":"web","image":"nginx:1.27"}]}}`
	create := httptest.NewRequest(http.MethodPost, "/api/v1/namespaces/default/pods", strings.NewReader(body))
	create.Header.Set("Authorization", "Bearer "+material.Token)
	create.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, create)
	if response.Code != http.StatusCreated {
		t.Fatalf("create fixture Pod: %d %s", response.Code, response.Body)
	}
	before, err := st.Rev()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.TLS == nil || r.Header.Get("Authorization") != "Bearer "+material.Token {
			t.Errorf("inspection request %s %s did not use authenticated HTTPS GET", r.Method, r.URL.Path)
		}
		s.ServeHTTP(w, r)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{material.Server}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	flags := []string{"--config-dir", configDir, "--listen", server.Listener.Addr().String()}
	var output bytes.Buffer
	wantSummary := fmt.Sprintf("cluster shearwater rev=%d node=<none> pods=0/1\n", before)
	if err := runInspect("status", flags, &output); err != nil || output.String() != wantSummary {
		t.Fatalf("status output %q, error %v", output.String(), err)
	}
	output.Reset()
	wantTree := wantSummary + "ns default\n  pod/waiting Pending 0/1 restarts=0\n"
	if err := runInspect("tree", append(flags, "-n", "default", "--root", "pods/waiting", "--problems"), &output); err != nil || output.String() != wantTree {
		t.Fatalf("tree output %q, error %v", output.String(), err)
	}
	output.Reset()
	err = runInspect("tree", append(flags, "--root", "deploy/missing"), &output)
	assertInspectError(t, err, "NOT_FOUND", "never")
	if output.Len() != 0 {
		t.Fatalf("failed command wrote partial output %q", output.String())
	}
	if after, err := st.Rev(); err != nil || after != before {
		t.Fatalf("reading changed the revision from %d to %d, error %v", before, after, err)
	}
	if entries, err := os.ReadDir(configDir); err != nil || len(entries) != 2 {
		t.Fatalf("inspection changed the client config directory: %v, error %v", entries, err)
	}
}

func TestReadInspectionRetriesChangedRevision(t *testing.T) {
	var nodeReads int
	get := func(_ context.Context, path string, out any) error {
		if path == "/api/v1/nodes" {
			nodeReads++
			if nodeReads == 1 {
				return json.Unmarshal([]byte(`{"metadata":{"resourceVersion":"10"},"items":[{"metadata":{"name":"old-node"}}]}`), out)
			}
		}
		return json.Unmarshal([]byte(`{"metadata":{"resourceVersion":"11"},"items":[]}`), out)
	}
	snapshot, err := readInspection(context.Background(), get, true)
	if err != nil || snapshot.Revision != "11" || len(snapshot.Nodes) != 0 || nodeReads != 2 {
		t.Fatalf("read inspection revision %q, nodes %v, node list reads %d, error %v", snapshot.Revision, snapshot.Nodes, nodeReads, err)
	}
}

func TestReadInspectionReturnsConflictOrTimeout(t *testing.T) {
	t.Run("every revision changes", func(t *testing.T) {
		var revision int
		get := func(_ context.Context, _ string, out any) error {
			revision++
			return json.Unmarshal([]byte(`{"metadata":{"resourceVersion":"`+strconv.Itoa(revision)+`"},"items":[]}`), out)
		}
		snapshot, err := readInspection(context.Background(), get, false)
		assertInspectError(t, err, "CONFLICT", "safe")
		if snapshot.Revision != "" {
			t.Fatalf("conflict returned revision %q", snapshot.Revision)
		}
	})
	t.Run("deadline on the second list", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		get := func(ctx context.Context, path string, out any) error {
			if path == "/api/v1/nodes" {
				return json.Unmarshal([]byte(`{"metadata":{"resourceVersion":"10"},"items":[]}`), out)
			}
			<-ctx.Done()
			return ctx.Err()
		}
		snapshot, err := readInspection(ctx, get, false)
		assertInspectError(t, err, "TIMEOUT", "safe")
		if snapshot.Revision != "" {
			t.Fatalf("timeout returned revision %q", snapshot.Revision)
		}
	})
}

func TestInspectFlags(t *testing.T) {
	for _, tt := range []struct {
		command string
		args    []string
	}{
		{"tree", []string{"--root", "deploy/bad/name"}},
		{"tree", []string{"-n", "Bad!"}},
		{"tree", []string{"extra"}},
		{"status", []string{"--problems"}},
		{"status", []string{"--listen", "192.168.1.10:6443"}},
	} {
		var output bytes.Buffer
		assertInspectError(t, runInspect(tt.command, tt.args, &output), "INVALID", "never")
		if output.Len() != 0 {
			t.Fatalf("%s %v wrote %q", tt.command, tt.args, output.String())
		}
	}
	var help bytes.Buffer
	if err := runInspect("tree", []string{"-h"}, &help); err != nil || !strings.Contains(help.String(), "-root") {
		t.Fatalf("tree help output %q, error %v", help.String(), err)
	}
}

func assertInspectError(t *testing.T, err error, code, retry string) {
	t.Helper()
	if err == nil || !strings.HasPrefix(err.Error(), code+" retry="+retry+": ") || !strings.Contains(err.Error(), "\nhint: ") {
		t.Fatalf("error %v, want %s retry=%s with a hint", err, code, retry)
	}
}
