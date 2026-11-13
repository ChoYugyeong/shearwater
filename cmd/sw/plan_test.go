package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shearwater/internal/api"
	"shearwater/internal/store"
)

func planTestServer(t *testing.T) (*agentClient, string, *store.Store) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &api.Server{Store: st, Token: "test-token"}
	if err = s.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(s)
	t.Cleanup(server.Close)
	if err = os.WriteFile(filepath.Join(root, "ca.crt"), []byte("test-ca-identity"), 0600); err != nil {
		t.Fatal(err)
	}
	return &agentClient{client: server.Client(), url: server.URL, token: s.Token}, root, st
}

var plannedPod = []byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"planned"},"spec":{"containers":[{"name":"web","image":"nginx:alpine"}]}}`)

func TestPlanApproveApplyAndNoBlindRetry(t *testing.T) {
	c, root, st := planTestServer(t)
	ctx := context.Background()
	before, _ := st.Rev()
	p, err := preparePlan(ctx, c, root, "127.0.0.1:6443", plannedPod)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := st.Rev()
	if before != after {
		t.Fatal("planning changed the database")
	}
	id := planHash(p)
	if err = exclusiveJSON(filepath.Join(root, id+".json"), p); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = executePlan(ctx, c, root, p.Listen, root, id, &out); err == nil {
		t.Fatal("applied without approval")
	}
	exclusiveJSON(filepath.Join(root, id+".approved.json"), map[string]string{"plan": id})
	if err = executePlan(ctx, c, root, p.Listen, root, id, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"state":"complete"`) {
		t.Fatal(out.String())
	}
	if err = executePlan(ctx, c, root, p.Listen, root, id, &out); err == nil {
		t.Fatal("reapplied a consumed plan")
	}
	if _, _, _, err = apiRequest(ctx, c, "GET", "/api/v1/namespaces/default/pods/planned", nil); err != nil {
		t.Fatal(err)
	}
}
func TestPlanRejectsConcurrentChangeAndIdentity(t *testing.T) {
	c, root, _ := planTestServer(t)
	ctx := context.Background()
	if _, _, _, err := apiRequest(ctx, c, "POST", "/api/v1/namespaces/default/pods", plannedPod); err != nil {
		t.Fatal(err)
	}
	changed := bytes.ReplaceAll(plannedPod, []byte("nginx:alpine"), []byte("nginx:stable"))
	p, err := preparePlan(ctx, c, root, "127.0.0.1:6443", changed)
	if err != nil {
		t.Fatal(err)
	}
	id := planHash(p)
	exclusiveJSON(filepath.Join(root, id+".json"), p)
	exclusiveJSON(filepath.Join(root, id+".approved.json"), map[string]string{"plan": id})
	// Another actor changes labels without changing the image. Exact review still
	// becomes stale, because overwriting that actor's metadata would lose work.
	b, _, _, err := apiRequest(ctx, c, "GET", p.Actions[0].Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := objectMap(b)
	objectMeta(v)["labels"] = map[string]string{"new": "actor"}
	b, _ = json.Marshal(v)
	if _, _, _, err = apiRequest(ctx, c, "PUT", p.Actions[0].Path, b); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = executePlan(ctx, c, root, p.Listen, root, id, &out); err == nil || !strings.Contains(err.Error(), "CONFLICT") {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, id+".receipt.json")); !os.IsNotExist(err) {
		t.Fatal("stale plan reserved an execution")
	}
	if err = validatePlanConnection(p, c, root, "127.0.0.1:1234"); err == nil {
		t.Fatal("accepted another endpoint")
	}
	p.Actions[0].Body = []byte(`{}`)
	os.WriteFile(filepath.Join(root, id+".json"), mustJSON(t, p), 0600)
	if _, err = loadPlan(root, id); err == nil {
		t.Fatal("accepted tampered plan")
	}
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestMCPProtocolAndReadOnlyBoundary(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}
{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":3,"method":"tools/list"}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"sw_apply","arguments":{"plan":"anything"}}}
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"sw_get","arguments":{"resource":"pods","config-dir":"/other"}}}
{"jsonrpc":"2.0","id":6,"method":"ping"}
`
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(input), &out, mcpOptions{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 6 {
		t.Fatal(out.String())
	}
	for _, line := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(lines[0], "Initialize first") || strings.Contains(lines[2], "sw_apply") || !strings.Contains(lines[3], `"isError":true`) || !strings.Contains(lines[4], "unknown argument") {
		t.Fatal(out.String())
	}
	for _, tool := range mcpTools(true) {
		if strings.Contains(tool.Name, "approve") {
			t.Fatal("MCP must not approve its own deployment")
		}
	}
	if _, err := mcpCall(mcpOptions{}, "sw_get", json.RawMessage(`{"resource":"--config-dir=/other"}`)); err == nil || !strings.Contains(err.Error(), "invalid resource") {
		t.Fatal("resource became a CLI flag", err)
	}
}

type failPlanWrite struct {
	base   http.RoundTripper
	writes int
}

func (r *failPlanWrite) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == "POST" && req.URL.Query().Get("dryRun") == "" {
		r.writes++
		if r.writes == 2 {
			return nil, errors.New("injected lost response on second write")
		}
	}
	return r.base.RoundTrip(req)
}

func TestPlanPartialFailureRecordsExactTargetsAndRefusesReplay(t *testing.T) {
	c, root, _ := planTestServer(t)
	ctx := context.Background()
	manifest := []byte(`{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"first"}},{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"second"}}]}`)
	p, err := preparePlan(ctx, c, root, "127.0.0.1:6443", manifest)
	if err != nil {
		t.Fatal(err)
	}
	id := planHash(p)
	for suffix, value := range map[string]any{".json": p, ".approved.json": map[string]string{"plan": id}} {
		if err = exclusiveJSON(filepath.Join(root, id+suffix), value); err != nil {
			t.Fatal(err)
		}
	}
	r := &failPlanWrite{base: c.client.Transport}
	c.client.Transport = r
	var out bytes.Buffer
	if err = executePlan(ctx, c, root, p.Listen, root, id, &out); err == nil {
		t.Fatal("expected second write failure")
	}
	var receipt applyReceipt
	if err = readPrivateJSON(filepath.Join(root, id+".receipt.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "uncertain-or-partial" || len(receipt.Applied) != 1 || receipt.Applied[0] != "/api/v1/namespaces/first" {
		t.Fatalf("missing exact partial result: %+v", receipt)
	}
	if _, _, _, err = apiRequest(ctx, c, "GET", "/api/v1/namespaces/first", nil); err != nil {
		t.Fatal("lost successful first creation", err)
	}
	if _, code, _, _ := apiRequest(ctx, c, "GET", "/api/v1/namespaces/second", nil); code != 404 {
		t.Fatal("second creation was unexpectedly applied", code)
	}
	if err = executePlan(ctx, c, root, p.Listen, root, id, &out); err == nil || r.writes != 2 {
		t.Fatal("retried partial deployment", err, r.writes)
	}
}

func TestLargePlanRemainsReviewableAndExpiredPlanIsRefused(t *testing.T) {
	c, root, _ := planTestServer(t)
	obj, _ := objectMap(plannedPod)
	objectMeta(obj)["annotations"] = map[string]string{"large": strings.Repeat("review this value; ", 16000)}
	p, err := preparePlan(context.Background(), c, root, "127.0.0.1:6443", mustJSON(t, obj))
	if err != nil {
		t.Fatal(err)
	}
	id := planHash(p)
	if err = exclusiveJSON(filepath.Join(root, id+".json"), p); err != nil {
		t.Fatal(err)
	}
	b, err := planPreview(id, p)
	if err != nil {
		t.Fatal(err)
	}
	var preview struct {
		Plan          string `json:"plan"`
		FullPreview   bool   `json:"fullPreview"`
		ReviewCommand string `json:"reviewCommand"`
	}
	if err = json.Unmarshal(b, &preview); err != nil || len(b) > 256<<10 || preview.Plan != id || preview.FullPreview || preview.ReviewCommand != "sw plan --plan="+id {
		t.Fatal("large preview lost exact review path", string(b), err)
	}
	saved, err := loadPlan(root, id)
	if err != nil {
		t.Fatal("full saved plan changed", err)
	}
	// The private file is pretty-printed; compare JSON values, not whitespace.
	got, _ := objectMap(saved.Actions[0].Body)
	want, _ := objectMap(p.Actions[0].Body)
	if !bytes.Equal(mustJSON(t, got), mustJSON(t, want)) {
		t.Fatal("full saved plan lost manifest content")
	}
	p.Expires = time.Now().Add(-time.Second)
	id = planHash(p)
	if err = exclusiveJSON(filepath.Join(root, id+".json"), p); err != nil {
		t.Fatal(err)
	}
	if _, err = loadPlan(root, id); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatal("accepted expired plan", err)
	}
}
