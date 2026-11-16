package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 실제 API와 SQLite를 사용하지만 컨테이너 런타임은 연결하지 않는다.
// 테스트마다 만든 임시 저장소만 사용하므로 기존 앱에는 접근하지 않는다.
func focusObject(t *testing.T, c *agentClient, method, path string, value any) map[string]any {
	t.Helper()
	var body []byte
	if value != nil {
		body, _ = json.Marshal(value)
	}
	b, _, _, err := apiRequest(context.Background(), c, method, path, body)
	if err != nil {
		t.Fatal(method, path, err)
	}
	obj, err := objectMap(b)
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func focusFixture(t *testing.T, task string) (mcpOptions, *agentClient, map[string]any) {
	t.Helper()
	c, root, _ := planTestServer(t)
	focusObject(t, c, "POST", "/api/v1/namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "bench"}})
	manifest := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "web", "namespace": "bench", "annotations": map[string]any{"private": "PRIVATE-ANNOTATION"}},
		"spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": map[string]any{"app": "web"}}, "template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "web"}}, "spec": map[string]any{
			"containers": []any{map[string]any{"name": "app", "image": "nginx:alpine", "env": []any{map[string]any{"name": "TOKEN", "value": "PRIVATE-ENV-ONLY"}}, "volumeMounts": []any{map[string]any{"name": "cache", "mountPath": "/cache"}}, "readinessProbe": map[string]any{"tcpSocket": map[string]any{"port": 8080}}}, map[string]any{"name": "sidecar", "image": "busybox:1.36"}}, "volumes": []any{map[string]any{"name": "cache", "emptyDir": map[string]any{}}}}}}}
	obj := focusObject(t, c, "POST", "/apis/apps/v1/namespaces/bench/deployments", manifest)
	obj["status"] = map[string]any{"observedGeneration": 1, "replicas": 1, "updatedReplicas": 1, "readyReplicas": 1, "availableReplicas": 1}
	obj = focusObject(t, c, "PUT", "/apis/apps/v1/namespaces/bench/deployments/web/status", obj)
	o := mcpOptions{Config: root, Listen: "127.0.0.1:6443", Plans: filepath.Join(root, "plans"), Write: true,
		Focus: &focusSession{Target: "deployment/web", Namespace: "bench", Task: task}}
	if task == "image" {
		o.Focus.Container = "app"
	}
	if err := normalizeFocus(&o); err != nil {
		t.Fatal(err)
	}
	if err := bindFocus(context.Background(), &o, c); err != nil {
		t.Fatal(err)
	}
	return o, c, obj
}

func focusRead(t *testing.T, o mcpOptions) focusStatus {
	t.Helper()
	text, err := mcpCall(o, "sw_target_status", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "PRIVATE-") {
		t.Fatal("private details leaked", text)
	}
	var v focusStatus
	if err = json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFocusedCatalogEnforcesScopeBeforeDispatch(t *testing.T) {
	for _, task := range []string{"inspect", "image", "scale"} {
		t.Run(task, func(t *testing.T) {
			o, _, _ := focusFixture(t, task)
			want := 1
			if task != "inspect" {
				want = 2
			}
			if len(availableMCPTools(o)) != want {
				t.Fatal("unexpected catalog", availableMCPTools(o))
			}
			for _, name := range []string{"sw_get", "sw_plan", "sw_apply", "sw_approve", "sw_observe", "sw_capabilities"} {
				if _, err := mcpCall(o, name, json.RawMessage(`{}`)); err == nil {
					t.Fatal("hidden tool executed", name)
				}
			}
			for _, raw := range []string{`{"namespace":"default"}`, `{"resource":"deploy/other"}`, `{"uid":"other"}`, `{"generation":2}`, `null`, `[]`} {
				if _, err := mcpCall(o, "sw_target_status", json.RawMessage(raw)); err == nil {
					t.Fatal("scope override accepted", raw)
				}
			}
			v := focusRead(t, o)
			if v.State != "met" || v.Namespace != "bench" || v.Resource != "deploy/web" || v.UID != v.Goal.UID || v.HTTP != "not_checked" {
				t.Fatal(v)
			}
		})
	}
}

func TestFocusedGoalDoesNotFollowLaterGenerationOrReplacement(t *testing.T) {
	o, c, obj := focusFixture(t, "image")
	original := o.Focus.goal
	obj["status"].(map[string]any)["observedGeneration"] = 0
	obj = focusObject(t, c, "PUT", o.Focus.path()+"/status", obj)
	if v := focusRead(t, o); v.State != "unknown" {
		t.Fatal(v)
	}
	obj["spec"].(map[string]any)["replicas"] = 2
	obj = focusObject(t, c, "PUT", o.Focus.path(), obj)
	if v := focusRead(t, o); v.State != "stale" || v.Generation == original.Generation {
		t.Fatal(v)
	}
	if _, err := mcpCall(o, "sw_target_image", json.RawMessage(`{"image":"nginx:stable"}`)); err == nil {
		t.Fatal("new generation accepted")
	}
	focusObject(t, c, "DELETE", o.Focus.path(), map[string]any{"gracePeriodSeconds": 0})
	missing := focusRead(t, o)
	if missing.State != "unknown" || missing.UID != "" || missing.Generation != 0 || missing.Goal != original {
		t.Fatal("missing object echoed goal as observation", missing)
	}
	delete(obj, "status")
	obj["metadata"] = map[string]any{"name": "web", "namespace": "bench"}
	obj = focusObject(t, c, "POST", "/apis/apps/v1/namespaces/bench/deployments", obj)
	if v := focusRead(t, o); v.State != "stale" || v.UID == original.UID || v.Goal != original {
		t.Fatal(v)
	}
	if _, err := mcpCall(o, "sw_target_image", json.RawMessage(`{"image":"nginx:stable"}`)); err == nil {
		t.Fatal("replacement accepted")
	}
	if err := bindFocus(context.Background(), &o, c); err == nil {
		t.Fatal("silent rebind accepted")
	}
}

func TestFocusedPlansPreserveUnrelatedFieldsAndAcceptStatusOnlyChanges(t *testing.T) {
	for _, task := range []string{"image", "scale"} {
		t.Run(task, func(t *testing.T) {
			o, c, obj := focusFixture(t, task)
			// 상태만 갱신되면 설정 세대는 그대로다. 최신 resourceVersion을
			// 내부에서 다시 읽어도 고정한 UID/세대는 바뀌지 않는다.
			obj["status"].(map[string]any)["readyReplicas"] = 0
			obj = focusObject(t, c, "PUT", o.Focus.path()+"/status", obj)
			before, _ := json.Marshal(obj)
			raw := `{"image":"nginx:stable"}`
			name := "sw_target_image"
			if task == "scale" {
				name, raw = "sw_target_scale", `{"replicas":3}`
			}
			result, err := mcpCall(o, name, json.RawMessage(raw))
			if err != nil {
				t.Fatal(err)
			}
			var saved struct {
				Plan    string `json:"plan"`
				Applied bool   `json:"applied"`
			}
			if json.Unmarshal([]byte(result), &saved) != nil || !focusPlanKnown(o, saved.Plan) || saved.Applied {
				t.Fatal(result)
			}
			var plan deploymentPlan
			if err := readPrivateJSON(filepath.Join(o.Plans, saved.Plan+".json"), &plan); err != nil {
				t.Fatal(err)
			}
			if len(plan.Actions) != 1 || plan.Actions[0].Method != "PUT" || plan.Actions[0].Path != o.Focus.path() {
				t.Fatal(plan)
			}
			body, err := objectMap(plan.Actions[0].Body)
			if err != nil {
				t.Fatal(err)
			}
			delete(obj, "status")
			delete(body, "status")
			if task == "image" {
				obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = "nginx:stable"
			} else {
				obj["spec"].(map[string]any)["replicas"] = float64(3)
			}
			if !reflect.DeepEqual(obj, body) {
				t.Fatal("other fields changed")
			}
			after := focusObject(t, c, "GET", o.Focus.path(), nil)
			afterBytes, _ := json.Marshal(after)
			if string(before) != string(afterBytes) {
				t.Fatal("plan changed API")
			}
			if focusPlanKnown(o, strings.Repeat("a", 64)) {
				t.Fatal("foreign plan accepted")
			}
		})
	}
}

func TestFocusedOptionsFailClosed(t *testing.T) {
	for _, f := range []focusSession{{Task: "inspect"}, {Target: "pods/web"}, {Target: "deploy/../web"}, {Target: "deploy/web", Namespace: "../other"}, {Target: "deploy/web", Task: "unknown"}, {Target: "deploy/web", UID: "uid"}, {Target: "deploy/web", Generation: 1}, {Target: "deploy/web", UID: "uid", Generation: -1}, {Target: "deploy/web", Container: "app"}} {
		o := mcpOptions{Focus: &f, Write: true}
		if err := normalizeFocus(&o); err == nil {
			t.Fatal("invalid focus accepted", f)
		}
	}
	o := mcpOptions{Focus: &focusSession{Target: "deploy/web", Task: "image"}}
	if err := normalizeFocus(&o); err == nil {
		t.Fatal("planning without write opt-in")
	}
	o = mcpOptions{Focus: &focusSession{}}
	if err := normalizeFocus(&o); err != nil || o.Focus != nil {
		t.Fatal("classic mode changed", err)
	}
}

func TestFocusedExplicitGoalAndContainerSelection(t *testing.T) {
	o, c, obj := focusFixture(t, "inspect")
	// 명시한 과거 목표는 현재 서버 값으로 덮어쓰면 안 된다.
	o.Focus = &focusSession{Target: "deploy/web", Namespace: "bench", Task: "inspect", UID: "original-object", Generation: 9}
	if err := bindFocus(context.Background(), &o, c); err != nil {
		t.Fatal(err)
	}
	if v := focusRead(t, o); v.State != "stale" || v.Goal.UID != "original-object" || v.UID == v.Goal.UID {
		t.Fatal(v)
	}
	for _, container := range []string{"", "missing"} {
		o.Focus = &focusSession{Target: "deploy/web", Namespace: "bench", Task: "image", Container: container}
		if err := bindFocus(context.Background(), &o, c); err == nil {
			t.Fatal("ambiguous/absent container accepted")
		}
	}
	// Deployment의 점 포함 이름도 지원하고, 컨테이너가 하나일 때만 자동 선택한다.
	delete(obj, "status")
	obj["metadata"] = map[string]any{"name": "web.v2", "namespace": "bench"}
	pod := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	pod["containers"] = pod["containers"].([]any)[:1]
	focusObject(t, c, "POST", "/apis/apps/v1/namespaces/bench/deployments", obj)
	o.Focus = &focusSession{Target: "deploy/web.v2", Namespace: "bench", Task: "image"}
	if err := bindFocus(context.Background(), &o, c); err != nil {
		t.Fatal(err)
	}
	if o.Focus.Container != "app" {
		t.Fatal(o.Focus.Container)
	}
	if _, err := mcpCall(o, "sw_target_image", json.RawMessage(`{"image":"nginx:stable"}`)); err != nil {
		t.Fatal(err)
	}
	o.Focus = &focusSession{Target: "deploy/absent", Namespace: "bench", Task: "inspect"}
	if err := bindFocus(context.Background(), &o, c); err == nil {
		t.Fatal("missing target silently pinned")
	}
	o.Focus.UID, o.Focus.Generation = "original-absent", 1
	if err := bindFocus(context.Background(), &o, c); err != nil {
		t.Fatal(err)
	}
	if v := focusRead(t, o); v.State != "unknown" || v.UID != "" || v.Generation != 0 {
		t.Fatal(v)
	}
}

type focusRaceTransport struct {
	base      http.RoundTripper
	afterRead func()
}

func (r *focusRaceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err == nil && req.Method == "GET" && r.afterRead != nil {
		after := r.afterRead
		r.afterRead = nil
		after()
	}
	return resp, err
}

func TestFocusedPlanRejectsChangeAfterIdentityCheck(t *testing.T) {
	o, c, obj := focusFixture(t, "image")
	base := c.client.Transport
	c.client.Transport = &focusRaceTransport{base: base, afterRead: func() {
		// 집중 도구의 첫 GET 직후 다른 운영자가 설정을 바꾸는 경합을 만든다.
		obj["spec"].(map[string]any)["replicas"] = 2
		other := *c
		other.client = &http.Client{Transport: base}
		focusObject(t, &other, "PUT", o.Focus.path(), obj)
	}}
	if _, err := mcpCall(o, "sw_target_image", json.RawMessage(`{"image":"nginx:stable"}`)); err == nil {
		t.Fatal("racing generation accepted")
	}
	if len(o.Focus.plans) != 0 {
		t.Fatal("conflicting plan was saved")
	}
}
