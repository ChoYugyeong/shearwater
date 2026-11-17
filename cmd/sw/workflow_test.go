package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shearwater/internal/agent"
)

func workflowFixture(t *testing.T) (*agentClient, *appWorkflow, mcpOptions) {
	t.Helper()
	c, root, _ := planTestServer(t)
	identity, err := connectionIdentity(c, root)
	if err != nil {
		t.Fatal(err)
	}
	w := &appWorkflow{Format: "shearwater.workflow/v1", Config: root, Listen: "127.0.0.1:6443", Plans: root, Identity: identity, Name: "web", Namespace: "default", Image: "nginx:1.27", Port: 80, HTTPPath: "/"}
	o := mcpOptions{Write: true, Workflow: &workflowSession{Path: filepath.Join(root, "workflow.json")}}
	if err = exclusiveJSON(o.Workflow.Path, w); err != nil {
		t.Fatal(err)
	}
	return c, w, o
}

func approveWorkflowFixture(t *testing.T, w *appWorkflow) {
	t.Helper()
	if err := exclusiveJSON(filepath.Join(w.Plans, w.Pending+".approved.json"), map[string]string{"plan": w.Pending}); err != nil {
		t.Fatal(err)
	}
}

func TestConnectRejectsInvalidHTTPBeforeCreatingState(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "new", "web.json")
	err := runConnect([]string{"--workflow=" + file, "--config-dir=" + root,
		"--name=web", "--image=nginx:1.27", "--http-path=//outside.example/"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "HTTP path") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(file)); !os.IsNotExist(err) {
		t.Fatal("invalid input created workflow state", err)
	}
}

// 모델 없이 실제 API와 SQLite로 승인 경계/실행 기록/원래 UID를 검사한다.
// 준비되지 않은 가짜 Deployment를 성공했다고 처리하지 않아야 한다.
func TestWorkflowPlanApprovalCrashResumeAndPinnedUID(t *testing.T) {
	c, w, o := workflowFixture(t)
	ctx := context.Background()
	result, err := executeWorkflow(o, c, w, "deploy", "")
	if err != nil || !strings.Contains(result, "awaiting_approval") {
		t.Fatal(result, err)
	}
	id := w.Pending
	if _, err = executeWorkflow(o, c, w, "deploy", ""); err == nil {
		t.Fatal("overwrote pending plan")
	}
	result, err = workflowContinue(ctx, c, w)
	if err != nil || !strings.Contains(result, "awaiting_approval") {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(filepath.Join(w.Plans, id+".receipt.json")); !os.IsNotExist(err) {
		t.Fatal("unapproved plan attempted")
	}
	approveWorkflowFixture(t, w)
	var out bytes.Buffer
	if err = executePlan(ctx, c, w.Config, w.Listen, w.Plans, id, &out); err != nil {
		t.Fatal(err)
	}
	// 작업 파일 저장 전에 종료된 상황이다. 최신 GET 대신 실행 기록을 읽는다.
	loaded, err := loadWorkflow(o.Workflow.Path)
	if err != nil {
		t.Fatal(err)
	}
	result, err = executeWorkflow(o, c, &loaded, "status", "")
	if err != nil {
		t.Fatal(result, err)
	}
	if loaded.UID == "" || loaded.ServiceUID == "" || loaded.Pending != "" || loaded.LastPlan != id || loaded.Goal.Generation != 1 {
		t.Fatal(loaded)
	}
	result, err = workflowVerify(ctx, c, &loaded)
	if err != nil || strings.Contains(result, `"state":"healthy"`) {
		t.Fatal(result, err)
	}
	if _, err = workflowPlan(ctx, c, &loaded, "recover", ""); err == nil {
		t.Fatal("guessed unverified recovery image")
	}
	// 최신 관찰이 같은 이름의 다른 UID여도 고정한 앱을 바꾸지 않는다.
	loaded.UID = "another-uid"
	loaded.Goal.UID = loaded.UID
	if _, err = loaded.deployment(ctx, c); err == nil {
		t.Fatal("accepted different UID")
	}
}

func TestWorkflowRecoveryOnlyPlansAndPreservesDataFields(t *testing.T) {
	c, w, o := workflowFixture(t)
	ctx := context.Background()
	if _, err := executeWorkflow(o, c, w, "deploy", ""); err != nil {
		t.Fatal(err)
	}
	approveWorkflowFixture(t, w)
	var out bytes.Buffer
	if err := executePlan(ctx, c, w.Config, w.Listen, w.Plans, w.Pending, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := w.consumeReceipt(c); err != nil {
		t.Fatal(err)
	}
	w.HealthyImage = "nginx:1.27"
	d, err := w.deployment(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(d)
	v, _ := objectMap(body)
	pod := v["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	app := pod["containers"].([]any)[0].(map[string]any)
	app["image"] = "missing.invalid/example:no-such-tag"
	app["env"] = []any{map[string]any{"name": "KEEP", "value": "data-sentinel"}}
	pod["volumes"] = []any{map[string]any{"name": "data", "emptyDir": map[string]any{}}}
	body, _ = json.Marshal(v)
	if _, _, _, err = apiRequest(ctx, c, "PUT", w.focus(c, 1).path(), body); err != nil {
		t.Fatal(err)
	}
	result, err := workflowVerify(ctx, c, w)
	if err != nil || !strings.Contains(result, `"state":"stale"`) {
		t.Fatal(result, err)
	}
	result, err = workflowPlan(ctx, c, w, "recover", "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := loadPlan(w.Plans, w.Pending)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Actions) != 1 || plan.Actions[0].UID != w.UID || !bytes.Contains(plan.Actions[0].Body, []byte("data-sentinel")) || !bytes.Contains(plan.Actions[0].Body, []byte("emptyDir")) {
		t.Fatal("recovery lost fields")
	}
	current, err := w.deployment(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if current.Spec.Template.Spec.Containers[0].Image != "missing.invalid/example:no-such-tag" {
		t.Fatal("recovery applied without approval")
	}
}

func TestWorkflowRejectsUncertainReceiptsAndWrongConnection(t *testing.T) {
	c, w, _ := workflowFixture(t)
	w.Pending = strings.Repeat("a", 64)
	r := applyReceipt{Plan: w.Pending, State: "attempted", Identity: w.Identity, Listen: w.Listen}
	if err := exclusiveJSON(filepath.Join(w.Plans, w.Pending+".receipt.json"), r); err != nil {
		t.Fatal(err)
	}
	if _, err := w.consumeReceipt(c); err == nil {
		t.Fatal("retried uncertain execution")
	}
	r.State = "complete"
	r.Identity = "different"
	r.Applied = []string{"default/deploy/web"}
	r.Goals = []agent.ReadinessGoal{{Namespace: "default", Resource: "deploy/web", UID: "x", Generation: 1}}
	if err := atomicJSON(filepath.Join(w.Plans, w.Pending+".receipt.json"), r); err != nil {
		t.Fatal(err)
	}
	if _, err := w.consumeReceipt(c); err == nil {
		t.Fatal("accepted other connection receipt")
	}
}

func TestWorkflowHTTPIsBoundedNoRedirectNoCredentials(t *testing.T) {
	for _, mode := range []string{"ok", "wrong-body", "redirect", "large", "error"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("credentials leaked")
				}
				switch mode {
				case "ok":
					io.WriteString(w, "expected")
				case "wrong-body":
					io.WriteString(w, "other")
				case "redirect":
					http.Redirect(w, r, "http://unrelated.invalid/", 302)
				case "large":
					io.WriteString(w, strings.Repeat("a", 65537))
				case "error":
					w.WriteHeader(503)
				}
			}))
			defer srv.Close()
			result := probeWorkflowHTTP(context.Background(), srv.URL, "expected")
			if (result.State == "passed") != (mode == "ok") {
				t.Fatal(result)
			}
			if mode == "redirect" && result.Status != 302 {
				t.Fatal("redirect followed", result)
			}
		})
	}
}

func TestWorkflowToolScopeAndLoadValidation(t *testing.T) {
	_, w, o := workflowFixture(t)
	if len(availableMCPTools(o)) != 1 || availableMCPTools(o)[0].Name != "sw_workflow" {
		t.Fatal("unexpected tools")
	}
	if _, err := mcpCall(o, "sw_apply", json.RawMessage(`{"plan":"x"}`)); err == nil {
		t.Fatal("legacy write exposed")
	}
	for _, raw := range []string{`{"action":"approve"}`, `{"action":"update"}`, `{"action":"status","image":"x"}`, `{"action":"status","name":"other"}`} {
		if _, err := mcpCall(o, "sw_workflow", json.RawMessage(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	o.Write = false
	if _, err := mcpCall(o, "sw_workflow", json.RawMessage(`{"action":"continue"}`)); err == nil {
		t.Fatal("read only apply")
	}
	for _, path := range []string{"https://other.invalid/", "//other.invalid/", "/path#fragment"} {
		w.HTTPPath = path
		if err := atomicJSON(o.Workflow.Path, w); err != nil {
			t.Fatal(err)
		}
		if _, err := loadWorkflow(o.Workflow.Path); err == nil {
			t.Fatal(path)
		}
	}
}

func TestWorkflowRepeatedImageCannotChangeSelectedIntent(t *testing.T) {
	w := appWorkflow{Image: "nginx@sha256:initial", HealthyImage: "nginx@sha256:verified"}
	for _, a := range []struct {
		action, image string
		valid         bool
	}{{"deploy", w.Image, true}, {"deploy", "", true}, {"deploy", "other", false}, {"recover", w.HealthyImage, true}, {"recover", "other", false}} {
		if (validateWorkflowImage(w, a.action, a.image) == nil) != a.valid {
			t.Fatal(a)
		}
	}
}

func TestWorkflowVerificationCannotSkipPendingExecution(t *testing.T) {
	c, w, o := workflowFixture(t)
	if _, err := executeWorkflow(o, c, w, "deploy", ""); err != nil {
		t.Fatal(err)
	}
	for _, approved := range []bool{false, true} {
		want := "awaiting_approval"
		if approved {
			approveWorkflowFixture(t, w)
			want = "approved_pending"
		}
		result, err := workflowVerify(context.Background(), c, w)
		if err != nil || !strings.Contains(result, `"state":"`+want+`"`) {
			t.Fatal(result, err)
		}
		if _, err = os.Stat(filepath.Join(w.Plans, w.Pending+".receipt.json")); !os.IsNotExist(err) {
			t.Fatal("verify executed a pending plan")
		}
	}
	if !strings.Contains(instructionsFor(o), "ALREADY LOCALLY APPROVED") {
		t.Fatal("resume instructions lost approval state")
	}
}
