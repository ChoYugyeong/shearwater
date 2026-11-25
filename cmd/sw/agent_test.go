package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"shearwater/internal/agent"
	"shearwater/internal/api"
	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/objects"
	"shearwater/internal/pki"
	"shearwater/internal/rbac"
	"shearwater/internal/store"
)

func startAgentTestServer(t *testing.T) (adminDir, listen string, st *store.Store, adminToken string) {
	t.Helper()
	mat, err := pki.Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adminDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(adminDir, "ca.crt"), mat.CACert, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adminDir, "admin.token"), []byte(mat.Token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.InitCredentials(); err != nil {
		t.Fatal(err)
	}
	if err := rbac.Bootstrap(st); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerUsers(&wiring{Store: st, Admin: mux})
	authn := &auth.Authenticator{AdminToken: mat.Token, Credentials: st}
	srv := &api.Server{
		Store:      st,
		Token:      mat.Token,
		Admin:      mux,
		Auth:       authn,
		Authorizer: rbac.New(st),
	}
	if err := srv.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{mat.Server}, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return adminDir, ts.Listener.Addr().String(), st, mat.Token
}

// parseCodexTOML parses the generated [mcp_servers.shearwater] block without external dependencies.
func parseCodexTOML(t *testing.T, data []byte) (command string, args []string) {
	t.Helper()
	inSection := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inSection = line == "[mcp_servers.shearwater]"
			continue
		}
		if !inSection {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("invalid TOML line: %q", line)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "command":
			var s string
			if err := json.Unmarshal([]byte(val), &s); err != nil {
				t.Fatalf("parse TOML command %q: %v", val, err)
			}
			command = s
		case "args":
			if err := json.Unmarshal([]byte(val), &args); err != nil {
				t.Fatalf("parse TOML args %q: %v", val, err)
			}
		}
	}
	return command, args
}

func TestAgentAddFilesModesAndRBAC(t *testing.T) {
	adminDir, listen, st, adminToken := startAgentTestServer(t)
	outDir := filepath.Join(t.TempDir(), "ops-bot-agent")
	var out bytes.Buffer
	args := []string{
		"add", "ops-bot",
		"--namespace", "team-a",
		"--out", outDir,
		"--config-dir", adminDir,
		"--listen", listen,
	}
	if err := runAgent(args, &out); err != nil {
		t.Fatalf("sw agent add failed: %v", err)
	}

	// Check output includes ready-to-run client lines (with --namespace) and credential warning.
	summary := out.String()
	for _, want := range []string{
		"created agent ops-bot (namespace=team-a, role=edit)",
		"claude mcp add shearwater --",
		"codex mcp add shearwater --",
		"gemini mcp add shearwater --",
		"qwen mcp add shearwater --",
		"--namespace team-a",
		"warning:",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("output missing %q:\n%s", want, summary)
		}
	}

	// Check directory and file modes, and ensure admin token never appears anywhere.
	swDir := filepath.Join(outDir, "sw")
	if _, err := os.Stat(filepath.Join(swDir, "admin.token")); !os.IsNotExist(err) {
		t.Fatalf("agent directory must not contain admin.token, stat err=%v", err)
	}
	expectedFiles := []string{
		"kubeconfig",
		"AGENT.md",
		filepath.Join("sw", "ca.crt"),
		filepath.Join("sw", "token"),
		filepath.Join("mcp", "claude.json"),
		filepath.Join("mcp", "codex.toml"),
		filepath.Join("mcp", "gemini.json"),
		filepath.Join("mcp", "qwen.json"),
		filepath.Join("skills", "shearwater-ops", "SKILL.md"),
		filepath.Join("skills", "shearwater-ops", "references", "deploy.md"),
	}
	for _, rel := range expectedFiles {
		path := filepath.Join(outDir, rel)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("missing expected file %s: %v", rel, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("file %s mode = %o, want 0600", rel, info.Mode().Perm())
		}
	}
	err := filepath.WalkDir(outDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			if info.Mode().Perm() != 0o700 {
				t.Fatalf("dir %s mode = %o, want 0700", path, info.Mode().Perm())
			}
			return nil
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("file %s mode = %o, want 0600", path, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), adminToken) {
			t.Fatalf("admin token leaked into %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Verify kubeconfig and token contents.
	tokenBytes, err := os.ReadFile(filepath.Join(swDir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	agentToken := strings.TrimSpace(string(tokenBytes))
	if agentToken == "" || agentToken == adminToken {
		t.Fatalf("unexpected agent token %q", agentToken)
	}
	kc, err := os.ReadFile(filepath.Join(outDir, "kubeconfig"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{agentToken, `namespace: "team-a"`, `user: "ops-bot"`, "https://" + listen} {
		if !strings.Contains(string(kc), want) {
			t.Fatalf("kubeconfig missing %q:\n%s", want, string(kc))
		}
	}

	// Verify AGENT.md mentions name, namespace, role, --namespace flag, approval, untrusted evidence, and skill.
	guideBytes, err := os.ReadFile(filepath.Join(outDir, "AGENT.md"))
	if err != nil {
		t.Fatal(err)
	}
	guide := string(guideBytes)
	for _, want := range []string{"ops-bot", "team-a", "edit", "--namespace team-a", "sw_observe", "sw_verify_plan", "sw task", "plan hash", "untrusted evidence", "skills/shearwater-ops/SKILL.md"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("AGENT.md missing %q", want)
		}
	}

	// Verify MCP JSON configs parse and point to the agent connection folder with --namespace.
	wantArgs := []string{"mcp", "--config-dir", swDir, "--listen", listen, "--namespace", "team-a"}
	for _, name := range []string{"claude.json", "gemini.json", "qwen.json"} {
		raw, err := os.ReadFile(filepath.Join(outDir, "mcp", name))
		if err != nil {
			t.Fatal(err)
		}
		var cfg agentMCPConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		srv, ok := cfg.MCPServers["shearwater"]
		if !ok {
			t.Fatalf("%s missing shearwater server", name)
		}
		if !filepath.IsAbs(srv.Command) {
			t.Fatalf("%s command %q is not absolute", name, srv.Command)
		}
		if !slices.Equal(srv.Args, wantArgs) {
			t.Fatalf("%s args = %v, want %v", name, srv.Args, wantArgs)
		}
	}

	// Verify Codex TOML parses and points to the agent connection folder.
	tomlBytes, err := os.ReadFile(filepath.Join(outDir, "mcp", "codex.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cmd, gotArgs := parseCodexTOML(t, tomlBytes)
	if !filepath.IsAbs(cmd) {
		t.Fatalf("codex.toml command %q is not absolute", cmd)
	}
	if !slices.Equal(gotArgs, wantArgs) {
		t.Fatalf("codex.toml args = %v, want %v", gotArgs, wantArgs)
	}

	// Verify server-side labels on namespace and RoleBinding.
	nsRec, err := st.Get(store.Key{Resource: "namespaces", Name: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	var ns apis.Namespace
	if err := json.Unmarshal(nsRec.Data, &ns); err != nil {
		t.Fatal(err)
	}
	if ns.Metadata.Labels[agentLabel] != "ops-bot" || ns.Metadata.Labels[securityLabel] != "baseline" {
		t.Fatalf("unexpected namespace labels: %v", ns.Metadata.Labels)
	}
	rbRec, err := st.Get(store.Key{Group: apis.RBACGroup, Resource: "rolebindings", Namespace: "team-a", Name: "sw-user.ops-bot"})
	if err != nil {
		t.Fatal(err)
	}
	var rb apis.RoleBinding
	if err := json.Unmarshal(rbRec.Data, &rb); err != nil {
		t.Fatal(err)
	}
	if rb.Metadata.Labels[agentLabel] != "ops-bot" || rb.Metadata.Labels[userLabel] != "ops-bot" || rb.RoleRef.Name != "edit" {
		t.Fatalf("unexpected RoleBinding: %+v", rb)
	}

	// Create a Pod in team-a using admin credentials and verify HTTP-level RBAC with agent token.
	adminClient, err := localAPI(adminDir, listen)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	podJSON := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web"},"spec":{"containers":[{"name":"app","image":"nginx:1.27"}]}}`
	if _, _, _, err := apiRequest(ctx, adminClient, "POST", "/api/v1/namespaces/team-a/pods", []byte(podJSON)); err != nil {
		t.Fatalf("create fixture pod: %v", err)
	}

	agentClient, err := localAPI(swDir, listen)
	if err != nil {
		t.Fatalf("localAPI(agent swDir) failed: %v", err)
	}
	if agentClient.token != agentToken {
		t.Fatalf("localAPI did not read sw/token")
	}
	if _, code, _, err := apiRequest(ctx, agentClient, "GET", "/api/v1/namespaces/team-a/pods", nil); err != nil || code != http.StatusOK {
		t.Fatalf("agent list pods in team-a: code=%d err=%v", code, err)
	}
	if _, code, _, err := apiRequest(ctx, agentClient, "GET", "/api/v1/namespaces/default/pods", nil); err == nil || code != http.StatusForbidden {
		t.Fatalf("agent list pods in default: code=%d err=%v, want 403", code, err)
	}

	// Verify MCP tools with the agent's connection folder.
	mcpOpts := mcpOptions{Config: swDir, Listen: listen, Plans: filepath.Join(swDir, "plans"), Write: true}
	if caps, err := mcpCall(mcpOpts, "sw_capabilities", nil); err != nil || !strings.Contains(caps, "shearwater.capabilities/v1") {
		t.Fatalf("sw_capabilities failed: %q err=%v", caps, err)
	}
	if got, err := mcpCall(mcpOpts, "sw_get", json.RawMessage(`{"resource":"pods","namespace":"team-a"}`)); err != nil || !strings.Contains(got, "pod/web") {
		t.Fatalf("sw_get in team-a failed: %q err=%v", got, err)
	}
	if _, err := mcpCall(mcpOpts, "sw_get", json.RawMessage(`{"resource":"pods","namespace":"default"}`)); err == nil || !strings.Contains(err.Error(), "UNAUTHORIZED") {
		t.Fatalf("sw_get in default should fail with UNAUTHORIZED, got %v", err)
	}
	if got, err := mcpCall(mcpOpts, "sw_events", json.RawMessage(`{"namespace":"team-a"}`)); err != nil || !strings.Contains(got, "events rev=") {
		t.Fatalf("sw_events in team-a failed: %q err=%v", got, err)
	}
	if got, err := mcpCall(mcpOpts, "sw_why", json.RawMessage(`{"resource":"pod/web","namespace":"team-a"}`)); err != nil || !strings.Contains(got, "pod/web") {
		t.Fatalf("sw_why in team-a failed: %q err=%v", got, err)
	}
	for _, tool := range []string{"sw_status", "sw_tree", "sw_observe"} {
		_, err := mcpCall(mcpOpts, tool, json.RawMessage(`{}`))
		if err == nil || !strings.Contains(err.Error(), "UNAUTHORIZED") || !strings.Contains(err.Error(), "403") {
			t.Fatalf("%s with namespace-scoped token should fail with UNAUTHORIZED 403, got %v", tool, err)
		}
	}
	planOut, err := mcpCall(mcpOpts, "sw_app_plan", json.RawMessage(`{"action":"create","name":"demo","namespace":"team-a","image":"nginx:1.27","port":8080}`))
	if err != nil || !strings.Contains(planOut, "shearwater.app-plan/v1") {
		t.Fatalf("sw_app_plan in team-a failed: %q err=%v", planOut, err)
	}
}

func TestAgentClientSelectionAndOverwriteRefusal(t *testing.T) {
	adminDir, listen, st, _ := startAgentTestServer(t)
	outDir := filepath.Join(t.TempDir(), "viewer-agent")
	var out bytes.Buffer
	err := runAgent([]string{
		"add", "viewer",
		"--namespace", "team-b",
		"--role", "view",
		"--client", "claude,codex",
		"--out", outDir,
		"--config-dir", adminDir,
		"--listen", listen,
	}, &out)
	if err != nil {
		t.Fatalf("sw agent add viewer failed: %v", err)
	}
	for _, want := range []string{"claude.json", "codex.toml"} {
		if _, err := os.Stat(filepath.Join(outDir, "mcp", want)); err != nil {
			t.Fatalf("expected %s: %v", want, err)
		}
	}
	for _, unwanted := range []string{"gemini.json", "qwen.json"} {
		if _, err := os.Stat(filepath.Join(outDir, "mcp", unwanted)); !os.IsNotExist(err) {
			t.Fatalf("unselected client %s should not exist", unwanted)
		}
	}
	if strings.Contains(out.String(), "gemini mcp add") || strings.Contains(out.String(), "qwen mcp add") {
		t.Fatalf("output listed unselected clients:\n%s", out.String())
	}

	// View agent cannot create a plan (dry-run POST returns 403).
	swDir := filepath.Join(outDir, "sw")
	_, err = mcpCall(mcpOptions{Config: swDir, Listen: listen, Plans: filepath.Join(swDir, "plans"), Write: true},
		"sw_app_plan", json.RawMessage(`{"action":"create","name":"demo","namespace":"team-b","image":"nginx:1.27","port":8080}`))
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("view agent sw_app_plan should fail with 403, got %v", err)
	}

	// Refusal to overwrite an existing non-empty directory without --force, before touching the server.
	out.Reset()
	err = runAgent([]string{
		"add", "second-bot",
		"--namespace", "team-b",
		"--out", outDir,
		"--config-dir", adminDir,
		"--listen", listen,
	}, &out)
	assertInspectError(t, err, "CONFLICT", "never")
	if _, err := st.Credential(userID("second-bot")); err != store.ErrNotFound {
		t.Fatalf("refused overwrite should not have created server credential, got %v", err)
	}

	// With --force, overwriting the existing non-empty directory succeeds.
	out.Reset()
	err = runAgent([]string{
		"add", "second-bot",
		"--namespace", "team-b",
		"--client", "gemini",
		"--out", outDir,
		"--force",
		"--config-dir", adminDir,
		"--listen", listen,
	}, &out)
	if err != nil {
		t.Fatalf("sw agent add --force failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "mcp", "gemini.json")); err != nil {
		t.Fatalf("expected gemini.json after --force: %v", err)
	}
	// Existing namespace team-b keeps original viewer label, not overwritten by second-bot.
	nsRec, err := st.Get(store.Key{Resource: "namespaces", Name: "team-b"})
	if err != nil {
		t.Fatal(err)
	}
	var ns apis.Namespace
	if err := json.Unmarshal(nsRec.Data, &ns); err != nil {
		t.Fatal(err)
	}
	if ns.Metadata.Labels[agentLabel] != "viewer" {
		t.Fatalf("existing namespace label should remain viewer, got %v", ns.Metadata.Labels)
	}
}

func TestAgentListAndRemove(t *testing.T) {
	adminDir, listen, st, _ := startAgentTestServer(t)
	adminClient, err := localAPI(adminDir, listen)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Create a regular (non-agent) user via the users API.
	if _, _, _, err := apiRequest(ctx, adminClient, "POST", usersPath, []byte(`{"name":"alice","namespace":"team-h","role":"view"}`)); err != nil {
		t.Fatalf("create regular user: %v", err)
	}

	outDir := filepath.Join(t.TempDir(), "ci-bot-agent")
	var out bytes.Buffer
	if err := runAgent([]string{
		"add", "ci-bot",
		"--namespace", "team-ci",
		"--role", "edit",
		"--client", "claude",
		"--out", outDir,
		"--config-dir", adminDir,
		"--listen", listen,
	}, &out); err != nil {
		t.Fatalf("add ci-bot: %v", err)
	}

	// Put a Pod in team-ci to confirm remove keeps both namespace and apps.
	podJSON := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"worker"},"spec":{"containers":[{"name":"app","image":"nginx:1.27"}]}}`
	if _, _, _, err := apiRequest(ctx, adminClient, "POST", "/api/v1/namespaces/team-ci/pods", []byte(podJSON)); err != nil {
		t.Fatalf("create pod in team-ci: %v", err)
	}

	// List shows only agents (ci-bot), not regular users (alice), with RFC3339 created time.
	out.Reset()
	if err := runAgent([]string{"list", "--config-dir", adminDir, "--listen", listen}, &out); err != nil {
		t.Fatalf("list: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected header + 1 agent row, got:\n%s", out.String())
	}
	if lines[0] != "NAME\tNAMESPACE\tROLE\tCREATED" {
		t.Fatalf("unexpected header %q", lines[0])
	}
	fields := strings.Split(lines[1], "\t")
	if len(fields) != 4 || fields[0] != "ci-bot" || fields[1] != "team-ci" || fields[2] != "edit" {
		t.Fatalf("unexpected row: %v", fields)
	}
	if _, err := time.Parse(time.RFC3339, fields[3]); err != nil {
		t.Fatalf("created time %q is not RFC3339: %v", fields[3], err)
	}
	if strings.Contains(out.String(), "alice") {
		t.Fatalf("non-agent user alice should not appear in sw agent list")
	}

	// Removing a non-agent user via sw agent remove must fail cleanly.
	out.Reset()
	err = runAgent([]string{"remove", "alice", "--config-dir", adminDir, "--listen", listen}, &out)
	assertInspectError(t, err, "INVALID", "never")

	// Remove the agent: token is revoked, binding is deleted, namespace and pod remain.
	out.Reset()
	if err := runAgent([]string{"remove", "ci-bot", "--config-dir", adminDir, "--listen", listen}, &out); err != nil {
		t.Fatalf("remove ci-bot: %v", err)
	}
	if !strings.Contains(out.String(), "namespace team-ci and its apps were kept") {
		t.Fatalf("remove output should say namespace and apps were kept: %q", out.String())
	}
	agentClient, err := localAPI(filepath.Join(outDir, "sw"), listen)
	if err != nil {
		t.Fatal(err)
	}
	if _, code, _, err := apiRequest(ctx, agentClient, "GET", "/api/v1/namespaces/team-ci/pods", nil); err == nil || code != http.StatusUnauthorized {
		t.Fatalf("removed agent token should return 401, got code=%d err=%v", code, err)
	}
	if _, err := st.Get(store.Key{Group: apis.RBACGroup, Resource: "rolebindings", Namespace: "team-ci", Name: "sw-user.ci-bot"}); err != store.ErrNotFound {
		t.Fatalf("agent RoleBinding should be removed, got %v", err)
	}
	if _, err := st.Get(store.Key{Resource: "namespaces", Name: "team-ci"}); err != nil {
		t.Fatalf("agent namespace should survive removal: %v", err)
	}
	if _, err := st.Get(store.Key{Resource: "pods", Namespace: "team-ci", Name: "worker"}); err != nil {
		t.Fatalf("agent namespace pod should survive removal: %v", err)
	}

	// List is now empty (header only).
	out.Reset()
	if err := runAgent([]string{"list", "--config-dir", adminDir, "--listen", listen}, &out); err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	if strings.TrimSpace(out.String()) != "NAME\tNAMESPACE\tROLE\tCREATED" {
		t.Fatalf("expected empty agent list, got:\n%s", out.String())
	}
}

func TestLocalAPIPrefersTokenOverAdminToken(t *testing.T) {
	mat, err := pki.Ensure(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), mat.CACert, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "admin.token"), []byte("admin-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := localAPI(dir, "127.0.0.1:6443")
	if err != nil || c.token != "admin-secret" {
		t.Fatalf("fallback to admin.token failed: client=%+v err=%v", c, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("agent-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = localAPI(dir, "127.0.0.1:6443")
	if err != nil || c.token != "agent-secret" {
		t.Fatalf("token file should take precedence over admin.token: client=%+v err=%v", c, err)
	}
	// An empty token file must fail rather than falling back to admin.token.
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := localAPI(dir, "127.0.0.1:6443"); err == nil {
		t.Fatal("empty token file must not fall back to admin.token")
	}
}

func TestAgentValidationErrors(t *testing.T) {
	adminDir, listen, _, _ := startAgentTestServer(t)
	base := []string{"--config-dir", adminDir, "--listen", listen}
	tmpOut := filepath.Join(t.TempDir(), "unused-agent")
	for i, args := range [][]string{
		{},
		{"unknown"},
		{"add"},
		{"add", auth.AdminName, "--namespace", "team-a", "--out", tmpOut},
		{"add", "bot", "--out", tmpOut},
		{"add", "bot", "--namespace", "Bad!", "--out", tmpOut},
		{"add", "bot", "--namespace", "team-a", "--role", "cluster-admin", "--out", tmpOut},
		{"add", "bot", "--namespace", "team-a", "--client", "unknown", "--out", tmpOut},
		{"list", "extra"},
		{"remove"},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			var out bytes.Buffer
			err := runAgent(append(args, base...), &out)
			assertInspectError(t, err, "INVALID", "never")
		})
	}
}

func TestAgentNamespaceScopedMCPToolsAndPlanVerification(t *testing.T) {
	adminDir, listen, st, _ := startAgentTestServer(t)
	adminClient, err := localAPI(adminDir, listen)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Create ops-bot scoped to team-a.
	outDir := filepath.Join(t.TempDir(), "ops-bot-agent")
	if err := runAgent([]string{
		"add", "ops-bot", "--namespace", "team-a", "--role", "edit",
		"--client", "claude", "--out", outDir,
		"--config-dir", adminDir, "--listen", listen,
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("sw agent add: %v", err)
	}
	swDir := filepath.Join(outDir, "sw")

	// Create team-b and seed Pods in both namespaces.
	if _, _, _, err := apiRequest(ctx, adminClient, "POST", "/api/v1/namespaces", []byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"team-b"}}`)); err != nil {
		t.Fatalf("create team-b: %v", err)
	}
	podBody, _, _, err := apiRequest(ctx, adminClient, "POST", "/api/v1/namespaces/team-a/pods",
		[]byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web"},"spec":{"containers":[{"name":"app","image":"nginx:1.27"}]}}`))
	if err != nil {
		t.Fatalf("create team-a pod: %v", err)
	}
	podObj, _ := objectMap(podBody)
	podMeta := objectMeta(podObj)
	podUID := textField(podMeta, "uid")
	podObj["status"] = map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	statusBytes, _ := json.Marshal(podObj)
	if _, _, _, err := apiRequest(ctx, adminClient, "PUT", "/api/v1/namespaces/team-a/pods/web/status", statusBytes); err != nil {
		t.Fatalf("update team-a pod status: %v", err)
	}
	if _, _, _, err := apiRequest(ctx, adminClient, "POST", "/api/v1/namespaces/team-b/pods",
		[]byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"secret-app"},"spec":{"containers":[{"name":"app","image":"nginx:1.27"}]}}`)); err != nil {
		t.Fatalf("create team-b pod: %v", err)
	}

	// Generic MCP server with --namespace team-a (as written by sw agent add).
	agentOpts := mcpOptions{Config: swDir, Listen: listen, Plans: filepath.Join(swDir, "plans"), Namespace: "team-a", Write: true}
	if instr := instructionsFor(agentOpts); !strings.Contains(instr, "Default namespace: team-a.") {
		t.Fatalf("instructions should mention default namespace: %q", instr)
	}

	// sw_status defaults to team-a, succeeds, and reports hidden nodes without node=<none>.
	statusOut, err := mcpCall(agentOpts, "sw_status", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("agent sw_status failed: %v", err)
	}
	if !strings.Contains(statusOut, "ns=team-a") || !strings.Contains(statusOut, "pods=1/1") || !strings.Contains(statusOut, agent.NodesHiddenNotice) || strings.Contains(statusOut, "node=<none>") {
		t.Fatalf("unexpected agent sw_status output: %q", statusOut)
	}

	// sw_tree defaults to team-a, succeeds, includes pod/web and hidden-node notice, excludes team-b.
	treeOut, err := mcpCall(agentOpts, "sw_tree", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("agent sw_tree failed: %v", err)
	}
	if !strings.Contains(treeOut, "ns team-a") || !strings.Contains(treeOut, "pod/web") || !strings.Contains(treeOut, agent.NodesHiddenNotice) || strings.Contains(treeOut, "team-b") || strings.Contains(treeOut, "secret-app") {
		t.Fatalf("unexpected agent sw_tree output:\n%s", treeOut)
	}

	// sw_observe defaults to team-a and returns team-a items with hidden-node note.
	obsOut, err := mcpCall(agentOpts, "sw_observe", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("agent sw_observe failed: %v", err)
	}
	var obs agent.Observation
	if err := json.Unmarshal([]byte(obsOut), &obs); err != nil {
		t.Fatalf("unmarshal observation: %v\n%s", err, obsOut)
	}
	if obs.Scope.Namespace != "team-a" || obs.Total != 1 || obs.Items[0].Resource != "pod/web" || !slices.Contains(obs.Notes, agent.NodesHiddenNotice) {
		t.Fatalf("unexpected observation: %+v", obs)
	}

	// sw_verify defaults to team-a and verifies exact UID/generation.
	verifyArgs, _ := json.Marshal(map[string]any{"resource": "pod/web", "uid": podUID, "generation": 1})
	verifyOut, err := mcpCall(agentOpts, "sw_verify", verifyArgs)
	if err != nil {
		t.Fatalf("agent sw_verify failed: %v", err)
	}
	var ver agent.Verification
	if err := json.Unmarshal([]byte(verifyOut), &ver); err != nil {
		t.Fatalf("unmarshal verification: %v", err)
	}
	if ver.State != "met" || ver.Goal.Namespace != "team-a" || !slices.Contains(ver.Observation.Notes, agent.NodesHiddenNotice) {
		t.Fatalf("unexpected verification: %+v", ver)
	}

	// sw_get, sw_events, sw_why, sw_changes all work with omitted namespace defaulting to team-a.
	if got, err := mcpCall(agentOpts, "sw_get", json.RawMessage(`{"resource":"pods"}`)); err != nil || !strings.Contains(got, "pod/web") {
		t.Fatalf("agent default-ns sw_get: %q err=%v", got, err)
	}
	if got, err := mcpCall(agentOpts, "sw_events", json.RawMessage(`{}`)); err != nil || !strings.Contains(got, "events rev=") {
		t.Fatalf("agent default-ns sw_events: %q err=%v", got, err)
	}
	if got, err := mcpCall(agentOpts, "sw_why", json.RawMessage(`{"resource":"pod/web"}`)); err != nil || !strings.Contains(got, "pod/web") {
		t.Fatalf("agent default-ns sw_why: %q err=%v", got, err)
	}
	if got, err := mcpCall(agentOpts, "sw_changes", json.RawMessage(`{}`)); err != nil || !strings.Contains(got, `"revision"`) {
		t.Fatalf("agent default-ns sw_changes: %q err=%v", got, err)
	}

	// Explicit namespace: team-a also works when MCP default namespace is NOT set.
	noDefaultOpts := mcpOptions{Config: swDir, Listen: listen, Plans: filepath.Join(swDir, "plans"), Write: true}
	if got, err := mcpCall(noDefaultOpts, "sw_status", json.RawMessage(`{"namespace":"team-a"}`)); err != nil || !strings.Contains(got, agent.NodesHiddenNotice) {
		t.Fatalf("explicit namespace sw_status failed: %q err=%v", got, err)
	}
	if got, err := mcpCall(noDefaultOpts, "sw_tree", json.RawMessage(`{"namespace":"team-a"}`)); err != nil || !strings.Contains(got, "pod/web") {
		t.Fatalf("explicit namespace sw_tree failed: %q err=%v", got, err)
	}
	if got, err := mcpCall(noDefaultOpts, "sw_observe", json.RawMessage(`{"namespace":"team-a"}`)); err != nil || !strings.Contains(got, "pod/web") {
		t.Fatalf("explicit namespace sw_observe failed: %q err=%v", got, err)
	}

	// Explicit namespace: team-b overrides the default team-a and returns HTTP 403 (UNAUTHORIZED).
	for _, tc := range []struct {
		tool string
		args string
	}{
		{"sw_status", `{"namespace":"team-b"}`},
		{"sw_tree", `{"namespace":"team-b"}`},
		{"sw_observe", `{"namespace":"team-b"}`},
		{"sw_verify", `{"resource":"pod/secret-app","namespace":"team-b","uid":"x","generation":1}`},
		{"sw_get", `{"resource":"pods","namespace":"team-b"}`},
		{"sw_events", `{"namespace":"team-b"}`},
		{"sw_why", `{"resource":"pod/secret-app","namespace":"team-b"}`},
		{"sw_app_plan", `{"action":"create","name":"demo","namespace":"team-b","image":"nginx:1.27","port":8080}`},
	} {
		_, err := mcpCall(agentOpts, tc.tool, json.RawMessage(tc.args))
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("%s for team-b should fail with 403, got %v", tc.tool, err)
		}
		if tc.tool != "sw_app_plan" {
			assertInspectError(t, err, "UNAUTHORIZED", "never")
		}
	}

	// sw_app_plan (default namespace team-a) -> local approve -> sw_apply -> post-apply verify & sw_verify_plan.
	planOut, err := mcpCall(agentOpts, "sw_app_plan", json.RawMessage(`{"action":"create","name":"demo","image":"nginx:1.27","port":8080}`))
	if err != nil {
		t.Fatalf("agent sw_app_plan failed: %v", err)
	}
	var savedPlan struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(planOut), &savedPlan); err != nil || !validPlanID(savedPlan.Plan) {
		t.Fatalf("invalid sw_app_plan output: %s err=%v", planOut, err)
	}
	if err := exclusiveJSON(filepath.Join(agentOpts.Plans, savedPlan.Plan+".approved.json"), map[string]string{"plan": savedPlan.Plan}); err != nil {
		t.Fatal(err)
	}
	applyArgs, _ := json.Marshal(map[string]string{"plan": savedPlan.Plan})
	applyOut, err := mcpCall(agentOpts, "sw_apply", applyArgs)
	if err != nil {
		t.Fatalf("agent sw_apply failed: %v", err)
	}
	var receipt applyReceipt
	if err := json.Unmarshal([]byte(applyOut), &receipt); err != nil {
		t.Fatalf("unmarshal apply receipt: %v\n%s", err, applyOut)
	}
	if receipt.State != "complete" || receipt.Verification == nil {
		t.Fatalf("unexpected apply receipt: %+v", receipt)
	}
	if strings.Contains(receipt.Verification.Reason, "observation_failed") || len(receipt.Verification.Targets) != 2 || !slices.Contains(receipt.Verification.Notes, agent.NodesHiddenNotice) {
		t.Fatalf("post-apply verification should succeed in team-a with hidden-node notice: %+v", receipt.Verification)
	}

	// Mark Deployment and Service ready (via EndpointSlice), then check sw_verify_plan returns met.
	depBody, _, _, err := apiRequest(ctx, adminClient, "GET", "/apis/apps/v1/namespaces/team-a/deployments/demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	depObj, _ := objectMap(depBody)
	depObj["status"] = map[string]any{"observedGeneration": 1, "replicas": 1, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1}
	depStatus, _ := json.Marshal(depObj)
	if _, _, _, err := apiRequest(ctx, adminClient, "PUT", "/apis/apps/v1/namespaces/team-a/deployments/demo/status", depStatus); err != nil {
		t.Fatal(err)
	}
	svcBody, _, _, err := apiRequest(ctx, adminClient, "GET", "/api/v1/namespaces/team-a/services/demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	svcObj, _ := objectMap(svcBody)
	svcMeta := objectMeta(svcObj)
	ready := true
	controller := true
	epSlice := &apis.EndpointSlice{
		Metadata: apis.ObjectMeta{
			Name:        "demo-1",
			Namespace:   "team-a",
			Labels:      map[string]string{apis.ServiceNameLabel: "demo"},
			Annotations: map[string]string{apis.ServiceGenerationAnnotation: "1"},
			OwnerReferences: []apis.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Service",
				Name:       "demo",
				UID:        textField(svcMeta, "uid"),
				Controller: &controller,
			}},
		},
		AddressType: "IPv4",
		Endpoints:   []apis.Endpoint{{Addresses: []string{"10.244.0.9"}, Conditions: apis.EndpointConditions{Ready: &ready}}},
	}
	epRes := apis.Lookup("discovery.k8s.io", "v1", "endpointslices")
	objects.Stamp(epRes, epSlice)
	if err := st.Write(false, func(tx *store.Tx) error {
		_, err := tx.Create(objects.Put(epRes, epSlice))
		return err
	}); err != nil {
		t.Fatalf("create endpointslice: %v", err)
	}
	planVerifyOut, err := mcpCall(agentOpts, "sw_verify_plan", applyArgs)
	if err != nil {
		t.Fatalf("agent sw_verify_plan failed: %v", err)
	}
	var pv planVerification
	if err := json.Unmarshal([]byte(planVerifyOut), &pv); err != nil {
		t.Fatalf("unmarshal plan verification: %v", err)
	}
	if pv.State != "met" || len(pv.Targets) != 2 || !slices.Contains(pv.Notes, agent.NodesHiddenNotice) {
		t.Fatalf("unexpected sw_verify_plan result: %+v", pv)
	}

	// Administrator cluster-wide answers remain unchanged (no hidden-node notice, sees both namespaces).
	adminOpts := mcpOptions{Config: adminDir, Listen: listen, Plans: filepath.Join(adminDir, "plans")}
	adminStatus, err := mcpCall(adminOpts, "sw_status", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(adminStatus, "node=<none>") || strings.Contains(adminStatus, agent.NodesHiddenNotice) || !strings.Contains(adminStatus, "pods=1/2") {
		t.Fatalf("admin cluster-wide sw_status changed: %q err=%v", adminStatus, err)
	}
	adminTree, err := mcpCall(adminOpts, "sw_tree", json.RawMessage(`{}`))
	if err != nil || !strings.Contains(adminTree, "ns team-a") || !strings.Contains(adminTree, "ns team-b") || strings.Contains(adminTree, agent.NodesHiddenNotice) {
		t.Fatalf("admin cluster-wide sw_tree changed:\n%s\nerr=%v", adminTree, err)
	}
	adminObsOut, err := mcpCall(adminOpts, "sw_observe", json.RawMessage(`{}`))
	if err != nil || strings.Contains(adminObsOut, `"notes"`) || !strings.Contains(adminObsOut, "secret-app") || !strings.Contains(adminObsOut, "pod/web") {
		t.Fatalf("admin cluster-wide sw_observe changed: %q err=%v", adminObsOut, err)
	}

	// runMCP flag parsing: --namespace in generic mode works, invalid/combined flags fail, focus -n stays intact.
	mcpInput := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}` + "\n"
	var mcpOut bytes.Buffer
	if err := runMCP([]string{"--config-dir", swDir, "--listen", listen, "--namespace", "team-a"}, strings.NewReader(mcpInput), &mcpOut); err != nil {
		t.Fatalf("runMCP with --namespace failed: %v", err)
	}
	if !strings.Contains(mcpOut.String(), "Default namespace: team-a.") {
		t.Fatalf("runMCP initialize did not include default namespace: %s", mcpOut.String())
	}
	assertInspectError(t, runMCP([]string{"--config-dir", swDir, "--listen", listen, "--namespace", "Bad!"}, strings.NewReader(""), &bytes.Buffer{}), "INVALID", "never")
	assertInspectError(t, runMCP([]string{"--config-dir", swDir, "--listen", listen, "--namespace", "team-a", "--target", "deploy/demo", "-n", "team-a"}, strings.NewReader(""), &bytes.Buffer{}), "INVALID", "never")
	mcpOut.Reset()
	if err := runMCP([]string{"--config-dir", swDir, "--listen", listen, "--target", "deploy/demo", "-n", "team-a"}, strings.NewReader(mcpInput), &mcpOut); err != nil {
		t.Fatalf("focus mode runMCP --target -n broke: %v", err)
	}
	if !strings.Contains(mcpOut.String(), "Use only the fixed target tools") {
		t.Fatalf("focus mode initialize output unexpected: %s", mcpOut.String())
	}
}
