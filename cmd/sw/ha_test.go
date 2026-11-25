package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"shearwater/internal/api"
	"shearwater/internal/auth"
	"shearwater/internal/ha"
	"shearwater/internal/nodeapi"
	"shearwater/internal/pki"
	"shearwater/internal/store"
)

func standbyConfig() ha.Config {
	return ha.Config{Self: "b", Role: ha.RoleStandby, Epoch: 5, Token: "ha-secret",
		Members: []ha.Member{{Name: "a", URL: "https://192.0.2.1:6443"}, {Name: "b", URL: "https://192.0.2.2:6443"}}}
}

func joinResponse() nodeapi.JoinResponse {
	return nodeapi.JoinResponse{NodeName: "b", NodeToken: "node-secret", ServingCert: "node-cert",
		PodCIDR: "10.244.1.0/24", ClusterCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/16",
		HA: &ha.Bundle{Config: standbyConfig(), CAKey: "ca-private", AdminToken: "admin-secret",
			Listen: "127.0.0.1:0", ServingKey: "witness-private", ServingCert: "witness-cert"}}
}

func TestJoinFilesPerRole(t *testing.T) {
	for _, role := range []string{ha.RoleWorker, ha.RoleControlPlane, ha.RoleWitness} {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			r := joinResponse()
			server := r.HA.Config.Members[0].URL
			if role != ha.RoleWorker {
				r.ControlPlanes = []string{server, r.HA.Config.Members[1].URL}
			} else {
				r.HA = nil
			}
			save := func() error {
				return writeJoinFiles(dir, role, server, "192.0.2.2", []byte("pinned-ca"), []byte("node-private"), r, false)
			}
			if err := save(); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"ca.crt": "pinned-ca"}
			if role != ha.RoleWitness {
				want["node.key"], want["node.token"], want["node.crt"] = "node-private", "node-secret\n", "node-cert"
				want[nodeConfigFile] = ""
				var cfg nodeConfig
				if err := readPrivateJSON(filepath.Join(dir, nodeConfigFile), &cfg); err != nil {
					t.Fatal(err)
				}
				servers := r.ControlPlanes
				if len(servers) == 0 {
					servers = []string{server}
				}
				if cfg.Server != server || !slices.Equal(cfg.Servers, servers) || cfg.NodeName != r.NodeName {
					t.Fatal("incorrect worker configuration or address list")
				}
			}
			switch role {
			case ha.RoleControlPlane:
				want["ca.key"], want["admin.token"] = "ca-private", "admin-secret\n"
				want[ha.ConfigFile], want[clusterFile] = "", ""
				cfg, err := ha.Load(dir)
				if err != nil || !reflect.DeepEqual(cfg, r.HA.Config) || readClusterConfig(dir).AdvertiseAddress != "192.0.2.2" {
					t.Fatal("incorrect standby configuration", err)
				}
			case ha.RoleWitness:
				want["witness.key"], want["witness.crt"], want[ha.WitnessFile] = "witness-private", "witness-cert", ""
				cfg, err := readWitnessConfig(dir)
				if err != nil || cfg.Listen != r.HA.Listen || !reflect.DeepEqual(cfg.Config, r.HA.Config) {
					t.Fatal("incorrect witness configuration", err)
				}
				b, err := os.ReadFile(filepath.Join(dir, ha.WitnessFile))
				if err != nil || bytes.Contains(b, []byte("ca-private")) || bytes.Contains(b, []byte("admin-secret")) || bytes.Contains(b, []byte("witness-private")) {
					t.Fatal("witness.json contains unexpected credentials", err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != len(want) {
				t.Fatalf("unexpected file set %v, %v", entries, err)
			}
			for name, contents := range want {
				path := filepath.Join(dir, name)
				body, err := os.ReadFile(path)
				if err != nil || (contents != "" && string(body) != contents) {
					t.Fatal("wrong or missing", name, err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if name != "ca.crt" && name != "node.crt" && info.Mode().Perm() != 0o600 {
					t.Errorf("%s has mode %o", name, info.Mode().Perm())
				}
			}
			if err := save(); err == nil || !strings.Contains(err.Error(), "--force") {
				t.Fatal("rejoin did not require --force", err)
			}
		})
	}
}

func TestWitnessJoinCommandChecksExistingConfigBeforeContact(t *testing.T) {
	dir := t.TempDir()
	caDir := t.TempDir()
	mat, err := pki.Ensure(caDir)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := pki.CAHash(mat.CACert)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case nodeapi.Prefix + "ca":
			w.Write(mat.CACert)
		case nodeapi.Prefix + "join":
			var req nodeapi.JoinRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Role != ha.RoleWitness || req.CSR != "" ||
				req.NodeName != "b" || req.InternalIP != "192.0.2.2" || r.Header.Get("Authorization") != "Bearer grant" {
				t.Error("incorrect witness enrollment request", err)
			}
			json.NewEncoder(w).Encode(joinResponse())
		default:
			http.NotFound(w, r)
		}
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{mat.Server}}
	server.StartTLS()
	defer server.Close()
	args := []string{server.URL, "--config-dir", dir, "--token", "grant", "--ca-hash", pin,
		"--node-name", "b", "--node-ip", "192.0.2.2", "--runtime", "none", "--witness"}
	if err := runJoin(args); err != nil {
		t.Fatal(err)
	}
	if _, err := readWitnessConfig(dir); err != nil || isWorker(dir) {
		t.Fatal("wrong role after sw join", err)
	}
	if err := runJoin(args); err == nil || !strings.Contains(err.Error(), "--force") || calls.Load() != 2 {
		t.Fatal("rejoin should refuse before sending the token", err)
	}
}

func TestForcedJoinReplacesRoleOnlyAfterValidation(t *testing.T) {
	dir := t.TempDir()
	r := joinResponse()
	save := func(role string, force bool) error {
		return writeJoinFiles(dir, role, "https://leader", "192.0.2.2", []byte("ca"), []byte("nodekey"), r, force)
	}
	if err := save(ha.RoleControlPlane, false); err != nil {
		t.Fatal(err)
	}
	r.HA.ServingKey = ""
	if err := save(ha.RoleWitness, true); err == nil {
		t.Fatal("accepted incomplete witness bundle")
	}
	if _, err := ha.Load(dir); err != nil || !isWorker(dir) {
		t.Fatal("failed replacement damaged the standby")
	}
	r.HA.ServingKey = "key"
	if err := save(ha.RoleWitness, true); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{nodeConfigFile, ha.ConfigFile, clusterFile, "node.key", "node.token", "ca.key", "admin.token"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s remained after joining as a witness", name)
		}
	}
}

func TestHAPromote(t *testing.T) {
	dir := t.TempDir()
	cfg := standbyConfig()
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"promote", "--config-dir", dir}
	path := filepath.Join(dir, ha.PromoteFile)
	if err := runHA(args, &out); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatal("missing force must explain the refusal", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unforced promotion created a request")
	}
	args = append(args, "--force")
	if err := runHA(args, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil || !strings.Contains(out.String(), "are lost.\nNever run this") {
		t.Fatal("force did not write the request and both warnings", err)
	}
	cfg.Role = ha.RoleLeader
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := runHA(args, &out); err == nil {
		t.Fatal("a leader accepted a promotion")
	}
}

func TestHAStatusStandbyJSON(t *testing.T) {
	dir := t.TempDir()
	cfg := standbyConfig()
	if err := cfg.Save(dir); err != nil {
		t.Fatal(err)
	}
	snap := ha.StandbyStatus{Role: ha.RoleStandby, Self: cfg.Self, Leader: "a", Epoch: 6, Seq: 42}
	if err := atomicJSON(filepath.Join(dir, ha.StatusFile), snap); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHA([]string{"status", "--config-dir", dir, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var got haStatusReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Role != ha.RoleStandby || got.Epoch != 6 || got.Leader != "a" ||
		got.Seq == nil || *got.Seq != 42 || got.InSync != nil || !reflect.DeepEqual(got.Members, cfg.Members) || strings.Contains(out.String(), cfg.Token) {
		t.Fatalf("unexpected standby status: %s, %v", &out, err)
	}
}

func TestWitnessServesAndPersistsStatus(t *testing.T) {
	dir := t.TempDir()
	// Issue the listener certificate under the same CA the clients trust.
	caDir := t.TempDir()
	mat, err := pki.Ensure(caDir)
	if err != nil {
		t.Fatal(err)
	}
	key, csr, err := pki.NewNodeKey("w")
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.SignNodeServing(caDir, csr, "w", []net.IP{net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	r := joinResponse()
	r.HA.Config.Self = "w"
	r.HA.ServingCert, r.HA.ServingKey = string(cert), string(key)
	if err := writeJoinFiles(dir, ha.RoleWitness, "", "", mat.CACert, nil, r, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		err := runWitness(ctx, dir, writer)
		writer.CloseWithError(err)
		done <- err
	}()
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(strings.TrimSpace(line), "Witness listening on ")
	hc, err := haHTTPClient(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.CloseIdleConnections()
	wc := ha.WitnessClient{URL: addr, Token: "wrong", Client: hc}
	if _, err := wc.Call(ctx, "state", ha.WitnessRequest{}); err == nil {
		t.Fatal("unauthenticated witness request succeeded")
	}
	wc.Token = r.HA.Config.Token
	reply, err := wc.Call(ctx, "heartbeat", ha.WitnessRequest{Leader: "a", Epoch: 5})
	if err != nil || !reply.OK {
		t.Fatal("witness rejected a valid heartbeat", err)
	}

	// The control plane independently authenticates both the leader and the witness.
	cfg := standbyConfig()
	cfg.Self, cfg.Role, cfg.Witness = "a", ha.RoleLeader, addr
	if err := cfg.Save(caDir); err != nil {
		t.Fatal(err)
	}
	leader := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" || req.URL.Path != ha.Prefix+"status" || req.Header.Get("Authorization") != "Bearer "+cfg.Token {
			t.Error("incorrect leader status request")
		}
		json.NewEncoder(w).Encode(ha.Status{Self: "a", Role: ha.RoleLeader, Epoch: 5, Seq: 27, InSync: true})
	}))
	leader.TLS = &tls.Config{Certificates: []tls.Certificate{mat.Server}}
	leader.StartTLS()
	defer leader.Close()
	var out bytes.Buffer
	if err := runHA([]string{"status", "--config-dir", caDir, "--listen", leader.Listener.Addr().String(), "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var st haStatusReport
	if err := json.Unmarshal(out.Bytes(), &st); err != nil || st.InSync == nil || !*st.InSync || st.Seq == nil || *st.Seq != 27 ||
		st.WitnessState == nil || st.WitnessState.Epoch != 5 || st.WitnessState.Leader != "a" {
		t.Fatal("leader and witness status", out.String(), err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runHA([]string{"status", "--config-dir", dir, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	st = haStatusReport{}
	if err := json.Unmarshal(out.Bytes(), &st); err != nil || st.Role != ha.RoleWitness || st.Leader != "a" || st.Epoch != 5 ||
		st.WitnessState == nil || !slices.Equal(st.WitnessState.InSync, []string{"a"}) || strings.Contains(out.String(), wc.Token) {
		t.Fatal("offline witness status", out.String(), err)
	}
}

func TestWorkerPersistsLearnedServers(t *testing.T) {
	dir := t.TempDir()
	mat, err := pki.Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	leader := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nodeapi.Prefix+"objects" || r.Header.Get("Authorization") != "Bearer node" {
			t.Error("unexpected worker request")
		}
		w.Header().Set("X-Shearwater-Control-Planes", "https://"+r.Host+",https://standby:6443")
		io.WriteString(w, `{"rev":1,"items":[]}`)
	}))
	leader.TLS = &tls.Config{Certificates: []tls.Certificate{mat.Server}}
	leader.StartTLS()
	defer leader.Close()
	// An existing node.json can have only the original server field.
	cfg := nodeConfig{Server: leader.URL, NodeName: "worker", PodCIDR: "10.244.2.0/24", Joined: time.Now().UTC()}
	if err := atomicJSON(filepath.Join(dir, nodeConfigFile), cfg); err != nil {
		t.Fatal(err)
	}
	files := workerFiles{cfg: cfg, token: "node", ca: mat.CACert}
	client, err := files.client(dir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer client.HTTP.CloseIdleConnections()
	if _, _, err := client.List("", "nodes", ""); err != nil {
		t.Fatal(err)
	}
	var saved nodeConfig
	if err := readPrivateJSON(filepath.Join(dir, nodeConfigFile), &saved); err != nil {
		t.Fatal(err)
	}
	cfg.Servers = []string{leader.URL, "https://standby:6443"}
	if !reflect.DeepEqual(cfg, saved) {
		t.Fatalf("persisted worker config = %+v", saved)
	}
	files.cfg = saved
	again, err := files.client(dir, t.Logf)
	if err != nil || !slices.Equal(again.URLs, cfg.Servers) {
		t.Fatal("worker restart did not reuse the advertised members", err)
	}
}

func TestRoleTokenAdminEndpoint(t *testing.T) {
	dir := t.TempDir()
	if _, err := pki.Ensure(dir); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.InitCredentials(); err != nil {
		t.Fatal(err)
	}
	w := &wiring{Store: st, Server: &api.Server{}, ConfigDir: dir, DataDir: dir, Admin: http.NewServeMux()}
	server := "https://192.0.2.1:6443"
	issue := func(role string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(joinTokenRequest{Role: role})
		rec := httptest.NewRecorder()
		w.Admin.ServeHTTP(rec, httptest.NewRequest("POST", joinTokensPath, bytes.NewReader(b)))
		return rec
	}
	wireNodes(w, server)
	if rec := issue(ha.RoleWitness); rec.Code != 409 || !strings.Contains(rec.Body.String(), "--advertise-address") {
		t.Fatal("issuing witness token without HA", rec.Code)
	}
	if rec := issue("leader"); rec.Code != 400 {
		t.Fatal("invalid token role accepted")
	}
	w.Admin = http.NewServeMux()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.HA = &haWiring{w: w, self: "a", advertise: netip.MustParseAddr("192.0.2.1"), port: "6443", ctx: ctx, logf: t.Logf}
	ns := wireNodes(w, server)
	if ns.JoinHA == nil || ns.ControlPlanes == nil {
		t.Fatal("HA callbacks not attached")
	}
	for _, role := range []string{ha.RoleWorker, ha.RoleControlPlane, ha.RoleWitness} {
		rec := issue(role)
		var token joinTokenResponse
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &token) != nil {
			t.Fatalf("could not issue a %s grant: HTTP %d", role, rec.Code)
		}
		_, id, _, ok := auth.Parse(token.Token)
		cred, err := st.Credential(id)
		var grant joinTokenRequest
		if !ok || err != nil || json.Unmarshal(cred.Data, &grant) != nil || grant.Role != role {
			t.Fatalf("wrong stored grant for %s", role)
		}
		if role != ha.RoleWorker {
			if _, err := ha.Load(dir); err != nil || !strings.HasSuffix(token.Command, "--"+role) {
				t.Fatalf("HA token missing enablement or flag for %s: %v", role, err)
			}
		}
	}
	for _, cmd := range []func() error{
		func() error { return runNode([]string{"token", "--control-plane", "--witness"}) },
		func() error { return runJoin([]string{server, "--control-plane", "--witness"}) },
	} {
		if err := cmd(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("ambiguous role flags: %v", err)
		}
	}
}
