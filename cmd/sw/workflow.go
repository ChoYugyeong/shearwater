package main

// 작업 파일은 AI의 기억을 대신한다. 앱 이름/UID, 연결 신원, 실행한 계획과
// HTTP까지 검증했던 이미지를 프로그램이 보관한다. 모델은 작업 종류와 새
// 이미지 값만 제안한다. 이 파일은 관리자 토큰을 가진 다른 프로그램까지
// 막는 권한 체계가 아니다. 파일과 인증 디렉터리는 같은 일반 사용자가 소유한다.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"shearwater/internal/agent"
	"shearwater/internal/apis"
)

type appWorkflow struct {
	Format       string              `json:"format"`
	Config       string              `json:"config"`
	Listen       string              `json:"listen"`
	Plans        string              `json:"plans"`
	Identity     string              `json:"identity"`
	Name         string              `json:"name"`
	Namespace    string              `json:"namespace"`
	Image        string              `json:"initialImage"`
	UpdateImage  string              `json:"stagedUpdateImage,omitempty"`
	Port         int32               `json:"port"`
	HTTPPath     string              `json:"httpPath"`
	Contains     string              `json:"expectedBody,omitempty"`
	UID          string              `json:"uid,omitempty"`
	ServiceUID   string              `json:"serviceUID,omitempty"`
	Goal         agent.ReadinessGoal `json:"goal"`
	Pending      string              `json:"pendingPlan,omitempty"`
	LastPlan     string              `json:"lastPlan,omitempty"`
	HealthyImage string              `json:"lastVerifiedImage,omitempty"`
}

type workflowSession struct {
	Path   string
	answer json.RawMessage
}

// 하나의 도구로 작업을 이어가지만 승인 도구는 제공하지 않는다. 숨긴 CLI
// 기능이 삭제된 것은 아니다. 사람이 문제를 살필 때는 기존 진단 명령을 쓴다.
func workflowTools(write bool) []mcpTool {
	actions := []string{"status", "diagnose", "verify"}
	properties := map[string]any{"action": map[string]any{"type": "string", "enum": actions}}
	if write {
		actions = append(actions, "deploy", "update", "recover", "continue")
		properties["action"].(map[string]any)["enum"] = actions
		properties["image"] = map[string]any{"type": "string", "description": "Omit for an update image already staged from a local build artifact. Otherwise required for update. Deploy/recover use the connected/verified image. A supplied value must exactly match any fixed image; never override or retype a staged digest."}
	}
	return []mcpTool{{"sw_workflow", "Operate the connected app only. status: workflow state; deploy: SAVE initial plan; update(image): SAVE image plan; diagnose: actual failure evidence; recover: SAVE plan for last HTTP-verified image. Creating a plan needs NO approval: when a plan is requested, actually call deploy/update/recover before ending or asking approval. Tool-call JSON in plain text does nothing. continue: execute locally approved pending plan once and verify; verify: poll without applying. Stop at awaiting_approval or healthy. Never guess IDs or claim success without healthy.", toolSchema(properties, "action"), map[string]any{"readOnlyHint": !write, "destructiveHint": write, "openWorldHint": false}}}
}

func addWorkflowFlag(f *flag.FlagSet, o *mcpOptions) {
	o.Workflow = &workflowSession{}
	f.StringVar(&o.Workflow.Path, "workflow", "", "private app workflow created by sw connect; carries connection and target")
}

func loadWorkflow(path string) (appWorkflow, error) {
	var w appWorkflow
	if !filepath.IsAbs(path) {
		return w, fmt.Errorf("workflow path must be absolute")
	}
	if err := readPrivateJSON(path, &w); err != nil {
		return w, err
	}
	if w.Format != "shearwater.workflow/v1" || !apis.IsDNS1123Label(w.Name) || !apis.IsDNS1123Label(w.Namespace) || w.Port < 1 || w.Port > 65535 || w.Image == "" || !filepath.IsAbs(w.Config) || !filepath.IsAbs(w.Plans) || w.Identity == "" {
		return w, fmt.Errorf("invalid workflow file")
	}
	if w.UpdateImage != "" && !validWorkflowImage(w.UpdateImage) {
		return w, fmt.Errorf("invalid staged update image")
	}
	if err := localListen(w.Listen); err != nil {
		return w, err
	}
	if err := validateWorkflowHTTP(w); err != nil {
		return w, err
	}
	if w.Pending != "" && !validPlanID(w.Pending) || w.LastPlan != "" && !validPlanID(w.LastPlan) {
		return w, fmt.Errorf("invalid workflow plan")
	}
	if w.UID != "" && (w.Goal.UID != w.UID || w.Goal.Resource != "deploy/"+w.Name || w.Goal.Namespace != w.Namespace || w.Goal.Generation < 1) {
		return w, fmt.Errorf("workflow goal identity mismatch")
	}
	return w, nil
}

// 잘못된 접속 확인 주소는 파일을 만들기 전에 거부한다. 실패한 연결 시도가
// 불완전한 작업 파일을 남겨 다음 정상 연결을 막지 않도록 읽을 때도 같은 검사를 쓴다.
func validateWorkflowHTTP(w appWorkflow) error {
	u, err := url.Parse(w.HTTPPath)
	if err != nil || !strings.HasPrefix(w.HTTPPath, "/") || strings.HasPrefix(w.HTTPPath, "//") || u.IsAbs() || u.Host != "" || u.Fragment != "" || len(w.HTTPPath) > 1024 || len(w.Contains) > 1024 {
		return fmt.Errorf("HTTP path must be a bounded local path")
	}
	return nil
}

func initWorkflow(o *mcpOptions) error {
	if o.Workflow == nil || o.Workflow.Path == "" {
		o.Workflow = nil
		return nil
	}
	if o.Focus != nil && (o.Focus.Target != "" || o.Focus.Task != "" || o.Focus.Namespace != "" || o.Focus.UID != "" || o.Focus.Generation != 0 || o.Focus.Container != "") {
		return fmt.Errorf("workflow and focused target options cannot be combined")
	}
	w, err := loadWorkflow(o.Workflow.Path)
	if err != nil {
		return err
	}
	// 파일에 기록한 환경이 기준이다. 호출 때 다른 접속 옵션을 덧붙여도
	// 조용히 다른 서버로 이동하지 않으며, 인증 신원도 호출마다 다시 검사한다.
	o.Config, o.Listen, o.Plans, o.Focus = w.Config, w.Listen, w.Plans, nil
	c, err := localAPI(w.Config, w.Listen)
	if err != nil {
		return err
	}
	identity, err := connectionIdentity(c, w.Config)
	if err != nil {
		return err
	}
	if identity != w.Identity {
		return fmt.Errorf("workflow server credentials changed; reconnect explicitly")
	}
	return nil
}

func (w appWorkflow) focus(c *agentClient, generation int64) *focusSession {
	return &focusSession{Target: "deploy/" + w.Name, Namespace: w.Namespace, client: c, goal: agent.ReadinessGoal{Namespace: w.Namespace, Resource: "deploy/" + w.Name, UID: w.UID, Generation: generation}}
}

func (w appWorkflow) deployment(ctx context.Context, c *agentClient) (*apis.Deployment, error) {
	d, err := readFocusDeployment(ctx, c, w.focus(c, w.Goal.Generation))
	if err != nil {
		return nil, err
	}
	if d != nil && (w.UID == "" || d.Metadata.UID != w.UID) {
		return nil, fmt.Errorf("STALE: app name belongs to an unselected UID; never adopt automatically")
	}
	if d == nil && w.UID != "" {
		return nil, fmt.Errorf("STALE: selected app was removed")
	}
	return d, nil
}

func workflowResult(w appWorkflow, state string, more map[string]any) string {
	v := map[string]any{"format": "shearwater.workflow-result/v1", "app": w.Namespace + "/" + w.Name, "state": state, "http": "not_checked"}
	if state == "awaiting_approval" {
		v["handoff"] = "local_approval"
	}
	if w.Pending != "" {
		v["plan"] = w.Pending
		v["approved"] = workflowApproved(w)
	}
	if w.UID != "" {
		v["uid"] = w.UID
		v["goalGeneration"] = w.Goal.Generation
	}
	for k, value := range more {
		v[k] = value
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func workflowApproved(w appWorkflow) bool {
	if w.Pending == "" {
		return false
	}
	var a struct {
		Plan string `json:"plan"`
	}
	return readPrivateJSON(filepath.Join(w.Plans, w.Pending+".approved.json"), &a) == nil && a.Plan == w.Pending
}

func workflowPendingResult(w appWorkflow) string {
	state, next := "awaiting_approval", "operator must review and approve the exact pending plan"
	if workflowApproved(w) {
		state, next = "approved_pending", "call sw_workflow with exactly {\"action\":\"continue\"} to execute the already approved plan and verify it"
	}
	return workflowResult(w, state, map[string]any{"applied": false, "next": next})
}

// 도구가 만든 결과와 현재 파일의 계획이 일치할 때만 사람에게 넘긴다.
// 모델이 문장에 쓴 JSON, 이전 질문의 계획, 이미 승인된 계획은 대상이 아니다.
func workflowNeedsApproval(path string, raw []byte) bool {
	var r struct {
		Format   string `json:"format"`
		State    string `json:"state"`
		Plan     string `json:"plan"`
		Approved *bool  `json:"approved"`
		Applied  *bool  `json:"applied"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Format != "shearwater.workflow-result/v1" || r.State != "awaiting_approval" || r.Plan == "" || r.Approved == nil || *r.Approved || r.Applied == nil || *r.Applied {
		return false
	}
	w, err := loadWorkflow(path)
	return err == nil && w.Pending == r.Plan && !workflowApproved(w)
}

func workflowPlan(ctx context.Context, c *agentClient, w *appWorkflow, action, image string) (string, error) {
	if w.Pending != "" {
		// 승인 대기 중인 계획을 모델의 다음 호출이 덮어쓰면 사람이 본 것과
		// 실행할 것이 달라진다. 새 의도는 로컬 discard 뒤에만 받는다.
		return "", fmt.Errorf("a plan is already pending; continue it, or locally discard that exact plan")
	}
	d, err := w.deployment(ctx, c)
	if err != nil {
		return "", err
	}
	i := appIntent{Name: w.Name, Namespace: w.Namespace, Action: "image", Image: image}
	if action == "deploy" {
		if d != nil {
			return "", fmt.Errorf("app already exists; use update")
		}
		i.Action, i.Image, i.Port = "create", w.Image, w.Port
	} else {
		if d == nil {
			return "", fmt.Errorf("deploy the connected app first")
		}
		if len(d.Spec.Template.Spec.Containers) != 1 {
			return "", fmt.Errorf("workflow image operations require one container; use an explicitly focused plan for multi-container apps")
		}
		i.UID, i.Revision, i.Container = d.Metadata.UID, d.Metadata.ResourceVersion, d.Spec.Template.Spec.Containers[0].Name
		if action == "recover" {
			if w.HealthyImage == "" {
				return "", fmt.Errorf("no HTTP-verified image recorded; do not guess a recovery image")
			}
			i.Image = w.HealthyImage
		}
	}
	p, err := prepareAppPlan(ctx, c, w.Config, w.Listen, i)
	if err != nil {
		return "", err
	}
	id := planHash(p)
	if err = exclusiveJSON(filepath.Join(w.Plans, id+".json"), p); err != nil {
		return "", err
	}
	w.Pending = id
	return workflowResult(*w, "awaiting_approval", map[string]any{"applied": false, "image": i.Image, "action": action, "next": "operator: sw workflow review --file=WORKFLOW, then approve --file=WORKFLOW --confirm=PLAN; agent: continue"}), nil
}

// 응답이 끊긴 뒤에도 실행 기록이 complete일 때만 신원을 복원한다.
// attempted/partial은 사람이 조사해야 하며 같은 쓰기를 재전송하지 않는다.
func (w *appWorkflow) consumeReceipt(c *agentClient) (bool, error) {
	if w.Pending == "" {
		return false, nil
	}
	var r applyReceipt
	err := readPrivateJSON(filepath.Join(w.Plans, w.Pending+".receipt.json"), &r)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.Plan != w.Pending || r.Identity != w.Identity || r.Listen != w.Listen || r.State != "complete" || len(r.Unverified) > 0 || len(r.Goals) != len(r.Applied) {
		return true, fmt.Errorf("uncertain or partial execution; inspect receipt, never automatically retry")
	}
	found := false
	for _, g := range r.Goals {
		if g.Namespace != w.Namespace {
			return true, fmt.Errorf("receipt outside connected app")
		}
		switch g.Resource {
		case "deploy/" + w.Name:
			if w.UID != "" && w.UID != g.UID {
				return true, fmt.Errorf("receipt replaced connected UID")
			}
			w.UID, w.Goal, found = g.UID, g, true
		case "svc/" + w.Name:
			if w.ServiceUID != "" && w.ServiceUID != g.UID {
				return true, fmt.Errorf("receipt replaced service UID")
			}
			w.ServiceUID = g.UID
		default:
			return true, fmt.Errorf("receipt outside connected app")
		}
	}
	if !found {
		return true, fmt.Errorf("receipt has no deployment goal")
	}
	w.LastPlan, w.Pending = w.Pending, ""
	return true, nil
}

func workflowContinue(ctx context.Context, c *agentClient, w *appWorkflow) (string, error) {
	if w.Pending != "" {
		consumed, err := w.consumeReceipt(c)
		if err != nil {
			return "", err
		}
		if !consumed {
			p, err := loadPlan(w.Plans, w.Pending)
			if err != nil {
				return "", err
			}
			if err = validatePlanConnection(p, c, w.Config, w.Listen); err != nil {
				return "", err
			}
			var a struct {
				Plan string `json:"plan"`
			}
			err = readPrivateJSON(filepath.Join(w.Plans, w.Pending+".approved.json"), &a)
			if errors.Is(err, os.ErrNotExist) {
				return workflowResult(*w, "awaiting_approval", map[string]any{"applied": false}), nil
			}
			if err != nil {
				return "", err
			}
			if a.Plan != w.Pending {
				return "", fmt.Errorf("approval mismatch")
			}
			// 저장한 계획도 앱 범위를 다시 검사한다. 다른 앱의 승인된 해시를
			// 작업 파일에 옮겨 넣었다고 그 앱을 실행할 수 있게 하지 않는다.
			for _, a := range p.Actions {
				base := "/namespaces/" + w.Namespace + "/"
				if a.Path != "/apis/apps/v1"+base+"deployments/"+w.Name && a.Path != "/apis/apps/v1"+base+"deployments" && a.Path != "/api/v1"+base+"services" {
					return "", fmt.Errorf("plan outside connected app")
				}
				var obj struct {
					Metadata apis.ObjectMeta `json:"metadata"`
				}
				if json.Unmarshal(a.Body, &obj) != nil || obj.Metadata.Name != w.Name || obj.Metadata.Namespace != w.Namespace {
					return "", fmt.Errorf("plan target mismatch")
				}
				if a.Method == "PUT" && (w.UID == "" || a.UID != w.UID) {
					return "", fmt.Errorf("plan UID mismatch")
				}
			}
			var out bytes.Buffer
			if err = executePlan(ctx, c, w.Config, w.Listen, w.Plans, w.Pending, &out); err != nil {
				return "", err
			}
			if _, err = w.consumeReceipt(c); err != nil {
				return "", err
			}
		}
	}
	// Compose의 up --wait처럼 준비를 제한 시간 동안 기다리되, 쓰기 요청은
	// 반복하지 않는다. 이후 호출에서도 저장된 원래 목표만 다시 검사한다.
	deadline := time.Now().Add(18 * time.Second)
	for {
		result, err := workflowVerify(ctx, c, w)
		if err != nil {
			return "", err
		}
		var answer struct {
			State string `json:"state"`
		}
		if err = json.Unmarshal([]byte(result), &answer); err != nil {
			return "", err
		}
		if answer.State != "not_met" && answer.State != "unknown" && answer.State != "http_failed" || time.Now().After(deadline) {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

type workflowHTTP struct {
	State           string `json:"state"`
	Status          int    `json:"status,omitempty"`
	SHA256          string `json:"bodySHA256,omitempty"`
	Bytes           int    `json:"bytes,omitempty"`
	Reason          string `json:"reason,omitempty"`
	serviceRevision string
}

// HTTP 목적지는 모델이 입력한 URL이 아니라 선택한 Service의 주소다.
// 토큰/쿠키를 보내지 않고 환경 프록시와 리다이렉트도 사용하지 않는다.
// 이 검사는 서버 내부의 Service 경로만 보장한다. 공개 도메인/TLS/로그인은
// 별도 사용자 목표이며 여기서 검사했다고 보고하지 않는다.
func workflowProbe(ctx context.Context, c *agentClient, w appWorkflow) (workflowHTTP, error) {
	var svc apis.Service
	path := "/api/v1/namespaces/" + w.Namespace + "/services/" + w.Name
	if err := c.get(ctx, path, &svc); err != nil {
		return workflowHTTP{}, err
	}
	if w.ServiceUID == "" || svc.Metadata.UID != w.ServiceUID || svc.Metadata.Name != w.Name || svc.Metadata.Namespace != w.Namespace {
		return workflowHTTP{}, fmt.Errorf("STALE: connected Service changed")
	}
	ip, parseErr := netip.ParseAddr(svc.Spec.ClusterIP)
	if parseErr != nil || !apis.ServiceCIDR.Contains(ip) || len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != w.Port || svc.Spec.Ports[0].Protocol != "TCP" || len(svc.Spec.Selector) != 1 || svc.Spec.Selector["app.kubernetes.io/name"] != w.Name {
		return workflowHTTP{}, fmt.Errorf("connected Service route changed or is outside supported IPv4 range")
	}
	address := "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(int(w.Port))) + w.HTTPPath
	checked := probeWorkflowHTTP(ctx, address, w.Contains)
	checked.serviceRevision = svc.Metadata.ResourceVersion
	return checked, nil
}

func probeWorkflowHTTP(ctx context.Context, address, contains string) workflowHTTP {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil {
		return workflowHTTP{State: "failed", Reason: "invalid_path"}
	}
	r, err := client.Do(req)
	if err != nil {
		return workflowHTTP{State: "failed", Reason: "request_failed"}
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	v := workflowHTTP{State: "failed", Status: r.StatusCode, Bytes: len(body)}
	if err != nil || len(body) > 64<<10 {
		v.Reason = "body_incomplete_or_over_budget"
		return v
	}
	sum := sha256.Sum256(body)
	v.SHA256 = hex.EncodeToString(sum[:])
	if r.StatusCode != 200 {
		v.Reason = "expected_HTTP_200"
		return v
	}
	if contains != "" && !bytes.Contains(body, []byte(contains)) {
		v.Reason = "expected_body_missing"
		return v
	}
	v.State = "passed"
	return v
}

func workflowVerify(ctx context.Context, c *agentClient, w *appWorkflow) (string, error) {
	// 대기 중인 새 변경을 아직 쓰지 않았다면 이전 세대가 정상이어도 새
	// 작업 완료가 아니다. verify는 절대로 실행을 대신하거나 성공을 추정하지 않는다.
	if w.Pending != "" {
		return workflowPendingResult(*w), nil
	}
	if w.UID == "" {
		return workflowResult(*w, "not_deployed", nil), nil
	}
	d, err := w.deployment(ctx, c)
	if err != nil {
		return "", err
	}
	if d.Metadata.Generation != w.Goal.Generation {
		return workflowResult(*w, "stale", map[string]any{"currentGeneration": d.Metadata.Generation, "next": "diagnose current app; a new update/recovery requires a new approved plan"}), nil
	}
	s := w.focus(c, w.Goal.Generation)
	snapshot, err := readFocusEvidence(ctx, c.get, s)
	if err != nil {
		return "", err
	}
	verification, err := snapshot.Verify(w.Goal, time.Now())
	if err != nil {
		return "", err
	}
	if verification.State != "met" {
		return workflowResult(*w, verification.State, map[string]any{"reason": verification.Reason, "next": "diagnose or verify again; do not apply again to wait"}), nil
	}
	httpCheck, err := workflowProbe(ctx, c, *w)
	if err != nil {
		return "", err
	}
	// 요청 도중 세대/Service가 바뀌면 방금 본 페이지를 원래 목표의 성공으로
	// 저장하지 않는다. 동시 변경을 없앤다는 뜻이 아니라 관측 경계를 확인한다.
	latest, err := w.deployment(ctx, c)
	if err != nil {
		return "", err
	}
	var svc apis.Service
	if err = c.get(ctx, "/api/v1/namespaces/"+w.Namespace+"/services/"+w.Name, &svc); err != nil {
		return "", err
	}
	if latest.Metadata.Generation != w.Goal.Generation || svc.Metadata.UID != w.ServiceUID || svc.Metadata.ResourceVersion != httpCheck.serviceRevision {
		return workflowResult(*w, "stale", map[string]any{"http": httpCheck}), nil
	}
	state := "http_failed"
	if httpCheck.State == "passed" {
		state = "healthy"
		if len(d.Spec.Template.Spec.Containers) == 1 {
			w.HealthyImage = d.Spec.Template.Spec.Containers[0].Image
		}
	}
	return workflowResult(*w, state, map[string]any{"http": httpCheck, "scope": "internal Service HTTP; public URL and data migrations not checked", "verifiedImage": w.HealthyImage}), nil
}

func callWorkflow(o mcpOptions, raw json.RawMessage) (string, error) {
	o.Workflow.answer = nil
	var a struct {
		Action string `json:"action"`
		Image  string `json:"image,omitempty"`
	}
	if err := decodeStrict(raw, &a); err != nil {
		return "", err
	}
	switch a.Action {
	case "status", "diagnose", "verify":
	case "deploy", "update", "recover", "continue":
		if !o.Write {
			return "", fmt.Errorf("workflow is read-only")
		}
	default:
		return "", fmt.Errorf("unknown workflow action")
	}
	if len(a.Image) > 1024 || a.Action != "update" && a.Action != "deploy" && a.Action != "recover" && a.Image != "" {
		return "", fmt.Errorf("only update/deploy/recover accept an image; omit it when already fixed")
	}
	lock, err := fileLock(filepath.Dir(o.Workflow.Path), filepath.Base(o.Workflow.Path)+".lock")
	if err != nil {
		return "", err
	}
	defer lock.Close()
	w, err := loadWorkflow(o.Workflow.Path)
	if err != nil {
		return "", err
	}
	c, err := localAPI(w.Config, w.Listen)
	if err != nil {
		return "", err
	}
	return executeWorkflow(o, c, &w, a.Action, a.Image)
}

func executeWorkflow(o mcpOptions, c *agentClient, w *appWorkflow, action, image string) (string, error) {
	if err := validateWorkflowImage(*w, action, image); err != nil {
		return "", err
	}
	if action == "update" && w.UpdateImage != "" {
		image = w.UpdateImage
	}
	identity, err := connectionIdentity(c, w.Config)
	if err != nil {
		return "", err
	}
	if identity != w.Identity {
		return "", fmt.Errorf("workflow connection identity changed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// 서버가 적용한 뒤 클라이언트가 종료됐어도 complete 영수증의 원래
	// 신원을 복원한다. 최신 GET을 근거로 다른 UID를 채택하지 않는다.
	if w.Pending != "" && action != "continue" {
		if _, err = w.consumeReceipt(c); err != nil {
			return "", err
		}
	}
	var result string
	switch action {
	case "deploy", "update", "recover":
		result, err = workflowPlan(ctx, c, w, action, image)
	case "continue":
		result, err = workflowContinue(ctx, c, w)
	case "verify":
		result, err = workflowVerify(ctx, c, w)
	case "diagnose":
		var d *apis.Deployment
		d, err = w.deployment(ctx, c)
		if err == nil && d == nil {
			result = workflowResult(*w, "not_deployed", nil)
		} else if err == nil {
			var text string
			text, err = diagnoseFocus(ctx, w.focus(c, d.Metadata.Generation))
			if err == nil {
				var facts map[string]any
				err = json.Unmarshal([]byte(text), &facts)
				// 진단만 끝내고 존재하지 않는 계획에 승인을 요구하던 실제 모델
				// 실패를 줄인다. 계획 저장과 외부 적용의 승인 경계를 명확히 한다.
				result = workflowResult(*w, "diagnosed", map[string]any{"evidence": facts["answer"], "lastVerifiedImage": w.HealthyImage, "next": "If recovery was requested, call recover NOW to SAVE its plan before ending. Creating the plan needs NO approval. Only applying that saved plan needs exact local approval."})
			}
		}
	case "status":
		var d *apis.Deployment
		d, err = w.deployment(ctx, c)
		state := "connected"
		if d == nil {
			state = "not_deployed"
		}
		if w.Pending != "" {
			state = "plan_pending"
		}
		result = workflowResult(*w, state, map[string]any{"lastVerifiedImage": w.HealthyImage, "actions": []string{"deploy", "update", "diagnose", "recover", "continue", "verify"}})
		if w.Pending != "" {
			result = workflowPendingResult(*w)
		}
	}
	if err != nil {
		return "", err
	}
	if err = atomicJSON(o.Workflow.Path, w); err != nil {
		return "", err
	}
	o.Workflow.answer = json.RawMessage(result)
	return result, nil
}

// 작은 모델은 사용자 문장의 이미지를 deploy 인자에도 반복할 수 있다.
// 그 값이 고정된 이미지와 정확히 같다면 받되, 다른 값은 무시하지 않고
// 명시적으로 거부한다. 복구 이미지도 같은 규칙으로 검증 기록에 묶는다.
func validateWorkflowImage(w appWorkflow, action, image string) error {
	if action == "update" {
		if w.UpdateImage == "" && image == "" {
			return fmt.Errorf("update requires image or a locally staged image artifact")
		}
		if w.UpdateImage != "" && image != "" && image != w.UpdateImage {
			return fmt.Errorf("update image is fixed by the local artifact; omit image and call exactly {\"action\":\"update\"}")
		}
	}
	if action == "deploy" && image != "" && image != w.Image {
		return fmt.Errorf("deploy image differs from the connected image; do not change the selected app intent")
	}
	if action == "recover" && image != "" && image != w.HealthyImage {
		return fmt.Errorf("recovery image must equal the last HTTP-verified image")
	}
	return nil
}

// connect는 클라이언트 작업 파일만 만든다. 서버/모델을 root로 켜거나
// 다른 AI 앱의 설정을 덮어쓰지 않는다. 출력의 command/args는 셸 문자열이 아니다.
func runConnect(args []string, out io.Writer) error {
	f := flag.NewFlagSet("connect", flag.ContinueOnError)
	w := appWorkflow{Format: "shearwater.workflow/v1"}
	file := f.String("workflow", "", "new absolute private workflow file")
	artifactFile := f.String("update-image-file", "", "optional absolute JSON build artifact selecting the next update image")
	f.StringVar(&w.Config, "config-dir", defaultConfigDir, "user-owned delegated credentials")
	f.StringVar(&w.Listen, "listen", defaultListen, "local HTTPS API")
	f.StringVar(&w.Name, "name", "", "one app name")
	f.StringVar(&w.Namespace, "n", "default", "app namespace")
	f.StringVar(&w.Image, "image", "", "initial built image; never execute repository scripts")
	port := f.Int("port", 80, "container and Service TCP port")
	f.StringVar(&w.HTTPPath, "http-path", "/", "internal Service HTTP path to verify")
	f.StringVar(&w.Contains, "expect-body", "", "optional response text, maximum 1024 bytes")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || !filepath.IsAbs(*file) || !filepath.IsAbs(w.Config) || !apis.IsDNS1123Label(w.Name) || !apis.IsDNS1123Label(w.Namespace) || w.Image == "" || len(w.Image) > 1024 || *port < 1 || *port > 65535 {
		return fmt.Errorf("connect requires absolute workflow/config paths, valid name/namespace/image/port")
	}
	w.Port = int32(*port)
	w.Plans = *file + ".plans"
	if err := validateWorkflowHTTP(w); err != nil {
		return err
	}
	if *artifactFile != "" {
		var err error
		w.UpdateImage, err = readWorkflowImageArtifact(*artifactFile)
		if err != nil {
			return err
		}
	}
	if err := privateDir(filepath.Dir(*file), true); err != nil {
		return err
	}
	if err := localListen(w.Listen); err != nil {
		return err
	}
	c, err := localAPI(w.Config, w.Listen)
	if err != nil {
		return err
	}
	w.Identity, err = connectionIdentity(c, w.Config)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()
	d, err := readFocusDeployment(ctx, c, w.focus(c, 0))
	if err != nil {
		return err
	}
	var svc apis.Service
	if d != nil {
		if err = c.get(ctx, "/api/v1/namespaces/"+w.Namespace+"/services/"+w.Name, &svc); err != nil {
			return err
		}
		w.UID, w.ServiceUID = d.Metadata.UID, svc.Metadata.UID
		w.Goal = agent.ReadinessGoal{Namespace: w.Namespace, Resource: "deploy/" + w.Name, UID: w.UID, Generation: d.Metadata.Generation}
	}
	if err = privateDir(w.Plans, true); err != nil {
		return err
	}
	if err = exclusiveJSON(*file, w); err != nil {
		return err
	}
	if _, err = loadWorkflow(*file); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"connected": true, "app": w.Namespace + "/" + w.Name, "workflow": *file, "mcpServers": map[string]any{"shearwater": map[string]any{"command": executable, "args": []string{"mcp", "--workflow=" + *file, "--allow-write"}}}, "next": "connect this MCP command in your AI client, then ask it to deploy; local exact-plan approval remains required"})
}

func runWorkflow(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("workflow requires status, stage-image, review, approve, or discard")
	}
	action := args[0]
	f := flag.NewFlagSet("workflow "+action, flag.ContinueOnError)
	file := f.String("file", "", "absolute workflow file")
	confirm := f.String("confirm", "", "exact reviewed plan hash (approve/discard)")
	artifactFile := f.String("image-file", "", "absolute JSON build artifact (stage-image only)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected workflow arguments")
	}
	if action != "stage-image" && *artifactFile != "" || action == "stage-image" && (*artifactFile == "" || *confirm != "") {
		return fmt.Errorf("stage-image requires --image-file and no --confirm; other actions cannot use --image-file")
	}
	w, err := loadWorkflow(*file)
	if err != nil {
		return err
	}
	if action == "status" {
		return json.NewEncoder(out).Encode(w)
	}
	lock, err := fileLock(filepath.Dir(*file), filepath.Base(*file)+".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	w, err = loadWorkflow(*file)
	if err != nil {
		return err
	}
	if action == "stage-image" {
		if w.Pending != "" {
			return fmt.Errorf("finish or locally discard the pending plan before staging another image")
		}
		w.UpdateImage, err = readWorkflowImageArtifact(*artifactFile)
		if err != nil {
			return err
		}
		if err = atomicJSON(*file, w); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"staged": true, "image": w.UpdateImage, "applied": false, "approved": false, "next": "ask the agent to update; it can omit image, then stop for exact plan approval"})
	}
	if w.Pending == "" {
		return fmt.Errorf("no pending plan")
	}
	switch action {
	case "review", "approve":
		command := "plan"
		if action == "approve" {
			command = "approve"
		}
		return runPlanCommand(command, []string{"--config-dir=" + w.Config, "--listen=" + w.Listen, "--plan-dir=" + w.Plans, "--plan=" + w.Pending, "--confirm=" + *confirm}, out)
	case "discard":
		if *confirm != w.Pending {
			return fmt.Errorf("repeat the pending hash with --confirm")
		}
		if _, err = os.Lstat(filepath.Join(w.Plans, w.Pending+".receipt.json")); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("attempted plans cannot be discarded; inspect receipt")
		}
		w.Pending = ""
		return atomicJSON(*file, w)
	default:
		return fmt.Errorf("unknown workflow command")
	}
}
