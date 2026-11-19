package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/edge"
	"shearwater/internal/objects"
	"shearwater/internal/pki"
	"shearwater/internal/store"
)

func TestEdgeEnableRequiresAgreementForHTTPS(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := runEdge([]string{"enable", "--config-dir", dir}, &out)
	if err == nil || !strings.Contains(err.Error(), "--agree-tos") {
		t.Fatal("HTTPS must not be enabled without agreeing to the authority's terms:", err)
	}
	if _, statErr := os.Stat(edgeConfigPath(dir)); statErr == nil {
		t.Fatal("a rejected setting was saved")
	}
	if err = runEdge([]string{"enable", "--config-dir", dir, "--http", ":80", "--https", ":80", "--agree-tos"}, &out); err == nil {
		t.Fatal("the same port for HTTP and HTTPS must be rejected")
	}
	if err = runEdge([]string{"enable", "--config-dir", dir, "--staging", "--agree-tos", "--email", "ops@example.com"}, &out); err != nil {
		t.Fatal(err)
	}
	c, err := readEdgeConfig(dir)
	if err != nil || !c.Enabled || c.HTTP != ":80" || c.HTTPS != ":443" || !strings.Contains(c.ACME.Directory, "staging") || c.ACME.Email != "ops@example.com" {
		t.Fatal("saved configuration is wrong", c, err)
	}
	if info, _ := os.Stat(edgeConfigPath(dir)); info.Mode().Perm() != 0o600 {
		t.Fatal("configuration must be private", info.Mode())
	}
	if !strings.Contains(out.String(), "restart the server") || !strings.Contains(out.String(), edge.HostAnnotation) {
		t.Fatal("enable does not say what to do next:", out.String())
	}
	if err = runEdge([]string{"disable", "--config-dir", dir}, &out); err != nil {
		t.Fatal(err)
	}
	if c, _ = readEdgeConfig(dir); c.Enabled || c.ACME.Email != "ops@example.com" {
		t.Fatal("disable must only switch the entry off", c)
	}
}

func TestPreparedEdgeServesAPublishedServiceAndReportsStatus(t *testing.T) {
	config, data := t.TempDir(), t.TempDir()
	st, err := store.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	o := opsOptions{ConfigDir: config, DataDir: data}
	if srv, err := prepareEdge(st, o); srv != nil || err != nil {
		t.Fatal("the public entry must be off without a configuration", srv, err)
	}
	var out bytes.Buffer
	if err = runEdge([]string{"status", "--config-dir", config, "--data-dir", data}, &out); err != nil || !strings.Contains(out.String(), "sw edge enable") {
		t.Fatal("status of a disabled entry must say how to enable it:", out.String(), err)
	}
	if err = runEdge([]string{"enable", "--config-dir", config, "--http", "127.0.0.1:0", "--https", "off"}, &out); err != nil {
		t.Fatal(err)
	}
	srv, err := prepareEdge(st, o)
	if err != nil || srv == nil || srv.HTTPS != nil {
		t.Fatal(srv, err)
	}
	app, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(app, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hello from the app") }))
	defer app.Close()
	srv.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, app.Addr().String())
	}
	s := apis.Service{Metadata: apis.ObjectMeta{Namespace: "default", Name: "web", Annotations: map[string]string{edge.HostAnnotation: "app.example.com"}},
		Spec: apis.ServiceSpec{Type: "ClusterIP", ClusterIP: "10.96.0.20", Ports: []apis.ServicePort{{Port: 80, Protocol: "TCP"}}}}
	res := apis.Lookup("", "v1", "services")
	objects.Stamp(res, &s)
	if err = st.Write(false, func(tx *store.Tx) error { _, e := tx.Create(objects.Put(res, &s)); return e }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go runPreparedEdge(ctx, srv, done)
	defer func() { cancel(); <-done }()
	req, _ := http.NewRequest("GET", "http://"+srv.HTTP.Addr().String()+"/", nil)
	req.Host = "app.example.com"
	body := ""
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && body != "hello from the app"; time.Sleep(20 * time.Millisecond) {
		if resp, e := http.DefaultClient.Do(req); e == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
		}
	}
	if body != "hello from the app" {
		t.Fatal("the published Service was not reachable through the entry:", body)
	}
	out.Reset()
	if err = runEdge([]string{"status", "--config-dir", config, "--data-dir", data}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"app.example.com", "default/web", "10.96.0.20:80"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status does not show %q:\n%s", want, out.String())
		}
	}
}

func TestEdgeStatusExplainsCertificateStates(t *testing.T) {
	var out bytes.Buffer
	retry := time.Now().Add(10 * time.Minute)
	st := edge.Status{HTTP: "[::]:80", HTTPS: "[::]:443",
		Routes: []edge.Route{{Host: "a.example.com", Namespace: "default", Service: "a", Backend: "10.96.0.2:80"}, {Host: "b.example.com", Namespace: "default", Service: "b", Backend: "10.96.0.3:80"}, {Host: "c.example.com", Namespace: "default", Service: "c", Backend: "10.96.0.4:80"}},
		Certs: map[string]edge.CertState{
			"a.example.com": {Ready: true, NotAfter: time.Date(2027, 2, 17, 0, 0, 0, 0, time.UTC)},
			"b.example.com": {LastError: "acme: DNS problem", RetryAt: retry},
		},
		Problems: []string{"Service default/x: host a.example.com already belongs to Service default/a"}}
	if err := printEdgeStatus(&out, st, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"valid until 2027-02-17", "failed, next try", "acme: DNS problem", "pending", "problem: Service default/x"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status does not show %q:\n%s", want, out.String())
		}
	}
}

func TestEdgeStatusMarksAnExpiredCertificate(t *testing.T) {
	var out bytes.Buffer
	st := edge.Status{HTTP: "[::]:80", HTTPS: "[::]:443",
		Routes: []edge.Route{{Host: "old.example.com", Namespace: "default", Service: "old", Backend: "10.96.0.2:80"}},
		Certs:  map[string]edge.CertState{"old.example.com": {Ready: true, NotAfter: time.Now().Add(-time.Hour), LastError: "acme: rate limited"}}}
	if err := printEdgeStatus(&out, st, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "EXPIRED") || !strings.Contains(out.String(), "rate limited") || strings.Contains(out.String(), "valid until") {
		t.Fatalf("an expired certificate must not read as valid:\n%s", out.String())
	}
}

func TestBackupKeepsThePublicEntrySettingKeyAndCertificates(t *testing.T) {
	base := t.TempDir()
	data, config := filepath.Join(base, "data"), filepath.Join(base, "config")
	os.Mkdir(data, 0700)
	st, err := store.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = pki.Ensure(config); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = runEdge([]string{"enable", "--config-dir", config, "--agree-tos", "--staging"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err = edge.LoadAccountKey(filepath.Join(data, "edge", "account.key")); err != nil {
		t.Fatal(err)
	}
	certs := filepath.Join(data, "edge", "certs")
	os.MkdirAll(certs, 0700)
	os.WriteFile(filepath.Join(certs, "app.example.com.crt"), []byte("certificate chain"), 0600)
	os.WriteFile(filepath.Join(certs, "app.example.com.key"), []byte("private key"), 0600)
	dest, target := filepath.Join(base, "backup"), filepath.Join(base, "restored")
	args := []string{"--data-dir=" + data, "--config-dir=" + config, "--run-dir=" + filepath.Join(base, "run"), "--log-dir=" + filepath.Join(base, "log"), "--runtime=none", "--output=" + dest}
	if err = runBackup(args, &out); err != nil {
		t.Fatal(err)
	}
	if err = runRestore([]string{"--from=" + dest, "--to=" + target}, &out); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"config/edge.json", "data/edge/account.key", "data/edge/certs/app.example.com.crt", "data/edge/certs/app.example.com.key"} {
		source := filepath.Join(base, rel)
		want, _ := os.ReadFile(source)
		got, err := os.ReadFile(filepath.Join(target, rel))
		if err != nil || len(want) == 0 || !bytes.Equal(got, want) {
			t.Fatalf("%s was not restored intact: %v", rel, err)
		}
		if info, _ := os.Stat(filepath.Join(target, rel)); info.Mode().Perm() != 0o600 {
			t.Fatalf("%s must stay private, got %v", rel, info.Mode())
		}
	}
	if c, err := readEdgeConfig(filepath.Join(target, "config")); err != nil || !c.Enabled || !c.ACME.AgreeTOS {
		t.Fatal("the restored setting does not enable the entry", c, err)
	}
}

func TestLastLogLineNamesTheStartupFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if lastLogLine(path) != "" {
		t.Fatal("a missing log must give no reason")
	}
	long := strings.Repeat("earlier line\n", 600)
	os.WriteFile(path, []byte(long+"sw: public HTTP: listen tcp :80: bind: address already in use\x1b[0m\n\n"), 0600)
	got := lastLogLine(path)
	if !strings.Contains(got, "public HTTP: listen tcp :80: bind: address already in use") || strings.ContainsRune(got, 0x1b) || strings.Contains(got, "earlier") {
		t.Fatalf("unexpected reason %q", got)
	}
}
