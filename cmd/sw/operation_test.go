package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"shearwater/internal/api"
	"shearwater/internal/operation"
	"shearwater/internal/store"
)

func taskTestServer(t *testing.T) (string, string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, e := store.Open(filepath.Join(dir, "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	s := &api.Server{Store: st, Token: "operator-only-secret"}
	if e = s.Bootstrap(); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewTLSServer(s)
	t.Cleanup(server.Close)
	if e = os.WriteFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "admin.token"), []byte(s.Token), 0600); e != nil {
		t.Fatal(e)
	}
	return dir, strings.TrimPrefix(server.URL, "https://"), st
}

func TestTaskCLIRestrictedCredentialsAndCrossClientResume(t *testing.T) {
	dir, listen, _ := taskTestServer(t)
	outDir := filepath.Join(dir, "agent")
	var out bytes.Buffer
	args := []string{"create", "--config-dir=" + dir, "--listen=" + listen, "--name=web", "--image=nginx:alpine", "--port=80", "--allow=deploy,update,recover", "--out=" + outDir}
	if e := runTask(args, &out); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(out.String(), "swop_") || strings.Contains(out.String(), "operator-only-secret") {
		t.Fatal("secret printed")
	}
	if _, e := os.Stat(filepath.Join(outDir, "admin.token")); !os.IsNotExist(e) {
		t.Fatal("admin credential delegated")
	}
	file := filepath.Join(outDir, "work.json")
	w, e := loadWorkflow(file)
	if e != nil {
		t.Fatal(e)
	}
	o := mcpOptions{Write: true, Workflow: &workflowSession{Path: file}}
	if e = initWorkflow(&o); e != nil {
		t.Fatal(e)
	}
	// MCP clients may cache this catalog across the whole app lifecycle. The
	// schema describes grant scope, while the server enforces the current stage.
	tools := delegatedTools(w, true)
	props := tools[0].InputSchema.(map[string]any)["properties"].(map[string]any)
	if _, exposed := props["image"]; !exposed {
		t.Fatal("future permitted updates missing from cached tool schema")
	}
	if got := props["action"].(map[string]any)["enum"].([]string); strings.Join(got, ",") != "status,diagnose,verify,continue,deploy,update,recover" {
		t.Fatal(got)
	}
	initialCatalog, _ := json.Marshal(tools)
	if _, e = mcpCall(o, "sw_workflow", json.RawMessage(`{"action":"deploy","image":"nginx:wrong"}`)); e == nil {
		t.Fatal("MCP changed the operator-selected initial image")
	}
	plan, e := mcpCall(o, "sw_workflow", json.RawMessage(`{"action":"deploy","image":"nginx:alpine"}`))
	if e != nil {
		t.Fatal(e)
	}
	if !workflowNeedsApproval(file, []byte(plan)) {
		t.Fatal("chat did not stop for approval")
	}
	var p operation.Result
	if e = json.Unmarshal([]byte(plan), &p); e != nil {
		t.Fatal(e)
	}
	if e = runWorkflow([]string{"approve", "--file=" + file, "--confirm=" + p.Plan}, io.Discard); e == nil {
		t.Fatal("delegated CLI approved")
	}
	if e = runTask([]string{"approve", "--id=" + w.Operation, "--config-dir=" + dir, "--listen=" + listen, "--confirm=" + p.Plan}, io.Discard); e != nil {
		t.Fatal(e)
	}
	if workflowNeedsApproval(file, []byte(plan)) {
		t.Fatal("old pending result claimed current approval was absent")
	}
	// Resume creates a NEW restricted credential directory and invalidates the
	// previous token while retaining the exact same app/task/approved plan.
	next := filepath.Join(dir, "agent-two")
	if e = runTask([]string{"resume", "--id=" + w.Operation, "--config-dir=" + dir, "--listen=" + listen, "--out=" + next}, io.Discard); e != nil {
		t.Fatal(e)
	}
	if _, e = remoteStatus(w); e == nil {
		t.Fatal("old token survived rotation")
	}
	w2, e := loadWorkflow(filepath.Join(next, "work.json"))
	if e != nil {
		t.Fatal(e)
	}
	status, e := remoteStatus(w2)
	if e != nil || status.Plan != p.Plan || !status.Approved {
		t.Fatal(status, e)
	}
	c, e := workflowAPI(w2)
	if e != nil {
		t.Fatal(e)
	}
	result, e := operationRequest(c, w2.Operation, operation.Request{Action: "continue", Plan: p.Plan})
	if e != nil || !result.Applied || result.State == "healthy" {
		t.Fatal(result, e)
	}
	if e = runTask([]string{"stage-image", "--id=" + w2.Operation, "--config-dir=" + dir, "--listen=" + listen, "--image=nginx:next"}, io.Discard); e != nil {
		t.Fatal(e)
	}
	// Staging must not silently change a cached MCP schema. A conflicting image
	// is instead rejected by the authoritative server, even with a valid token.
	props = delegatedTools(w2, true)[0].InputSchema.(map[string]any)["properties"].(map[string]any)
	currentCatalog, _ := json.Marshal(delegatedTools(w2, true))
	if !bytes.Equal(initialCatalog, currentCatalog) {
		t.Fatal("catalog changed during a long-lived MCP connection")
	}
	if got := props["action"].(map[string]any)["enum"].([]string); strings.Join(got, ",") != "status,diagnose,verify,continue,deploy,update,recover" {
		t.Fatal(got)
	}
	if _, e = operationRequest(c, w2.Operation, operation.Request{Action: "update", Image: "nginx:wrong"}); e == nil {
		t.Fatal("a conflicting staged image was accepted")
	}
	// Restoring a client must not grant generic API access or operator actions.
	_, code, _, _ := apiRequest(context.Background(), c, "GET", "/api/v1/pods", nil)
	if code != 401 {
		t.Fatal(code)
	}
	if e = runTask(args, io.Discard); e == nil {
		t.Fatal("existing credential directory overwritten")
	}
}

func TestTaskToolsReflectScopeAndNeverExposeApproval(t *testing.T) {
	w := appWorkflow{Allowed: []string{"update"}}
	tools := delegatedTools(w, true)
	actions := tools[0].InputSchema.(map[string]any)["properties"].(map[string]any)["action"].(map[string]any)["enum"].([]string)
	if strings.Join(actions, ",") != "status,diagnose,verify,continue,update" {
		t.Fatal(actions)
	}
	for _, tool := range tools {
		if tool.Name != "sw_workflow" {
			t.Fatal(tool.Name)
		}
	}
}

func TestContinueRetainsItsReceiptAcrossReadOnlyReadinessPolling(t *testing.T) {
	for _, change := range []string{"same", "new-generation", "new-pending-plan"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			plan, id := strings.Repeat("a", 64), strings.Repeat("b", 32)
			var count atomic.Int32
			requests := make(chan operation.Request, 3)
			server := httptest.NewTLSServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
				var q operation.Request
				if req.Header.Get("Authorization") != "Bearer swop_test" || json.NewDecoder(req.Body).Decode(&q) != nil {
					t.Error("invalid authenticated request")
					out.WriteHeader(400)
					return
				}
				requests <- q
				r := operation.Result{Format: "shearwater.workflow-result/v1", Operation: id, UID: "selected-app", Generation: 2, LastPlan: plan, Allowed: []string{"update"}, State: "not_met", HTTP: "not_checked"}
				if count.Add(1) == 1 {
					r.Plan, r.Applied, r.Approved = plan, true, true
				} else {
					r.State, r.Applied, r.VerifiedPods = "healthy", true, 1
					r.ProofID, r.VerifiedImage, r.HTTP = strings.Repeat("c", 64), "example/app:checked", map[string]any{"state": "passed"}
					if change == "new-generation" {
						r.Generation, r.LastPlan = 3, strings.Repeat("d", 64)
					} else if change == "new-pending-plan" {
						r.State, r.Review = "awaiting_approval", &operation.Plan{Hash: strings.Repeat("d", 64)}
					}
				}
				json.NewEncoder(out).Encode(r)
			}))
			defer server.Close()
			if e := os.WriteFile(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(dir, "delegated.token"), []byte("swop_test"), 0600); e != nil {
				t.Fatal(e)
			}
			w := appWorkflow{Format: "shearwater.workflow/v2", Config: dir, Listen: strings.TrimPrefix(server.URL, "https://"), Operation: id, Pending: plan}
			c, e := workflowAPI(w)
			if e != nil {
				t.Fatal(e)
			}
			w.Identity, e = connectionIdentity(c, dir)
			if e != nil {
				t.Fatal(e)
			}
			o := mcpOptions{Write: true, Workflow: &workflowSession{Path: filepath.Join(dir, "work.json")}}
			raw, e := callRemoteWorkflow(o, w, "continue", "")
			if e != nil {
				t.Fatal(e)
			}
			if count.Load() != 2 {
				t.Fatal("unexpected polling count", count.Load())
			}
			first, second := <-requests, <-requests
			if first.Action != "continue" || first.Plan != plan || second.Action != "verify" || second.Plan != "" {
				t.Fatal("polling repeated application", first, second)
			}
			var result operation.Result
			if json.Unmarshal([]byte(raw), &result) != nil {
				t.Fatal(raw)
			}
			complete := workflowAppliedAndHealthy([]byte(`{"action":"continue"}`), []byte(raw))
			if change == "same" {
				if !complete || result.Plan != plan || !result.Applied || !result.Approved {
					t.Fatal("the applied receipt was lost while waiting", raw)
				}
			} else if complete || result.State != "superseded" || result.ProofID != "" {
				t.Fatal("a later goal was attached to this receipt", raw)
			}
		})
	}
}
