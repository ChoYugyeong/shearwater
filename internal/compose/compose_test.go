package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"shearwater/internal/operation"
	"shearwater/internal/store"
)

const (
	tagged   = "nginx:1.28.0-alpine"
	pinnedV1 = "nginx@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	pinnedV2 = "nginx@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// fakeHost is dockerd plus the compose CLI in memory. `up` recreates the
// service's containers from the override file, as compose would.
type fakeHost struct {
	t         *testing.T
	service   map[string]any // the user's normalized service config
	conts     []Container
	next      int
	ups       int
	pullFail  string // image whose pull fails without touching containers
	partial   bool   // next up removes containers, then fails
	repoByID  map[string][]string
	healthy   bool
	extraArgs [][]string
}

func newHost(t *testing.T) *fakeHost {
	h := &fakeHost{t: t, healthy: true, repoByID: map[string][]string{}}
	h.service = map[string]any{
		"image":       tagged,
		"environment": map[string]string{"MODE": "prod"},
		"ports":       []map[string]any{{"target": 80, "published": "18080", "host_ip": "0.0.0.0", "protocol": "tcp", "mode": "ingress"}},
	}
	return h
}

func (h *fakeHost) add(image string, labels map[string]string) {
	h.next++
	c := Container{ID: fmt.Sprintf("c%02d", h.next), Name: fmt.Sprintf("/shop-web-%d", h.next), Image: "sha256:img-" + image}
	c.State.Running, c.State.Status = true, "running"
	c.Config.Image = image
	c.Config.Labels = map[string]string{projectLabel: "shop", serviceLabel: "web"}
	for k, v := range labels {
		c.Config.Labels[k] = v
	}
	c.NetworkSettings.Networks = map[string]struct {
		IPAddress string `json:"IPAddress"`
	}{"shop_default": {IPAddress: fmt.Sprintf("172.28.0.%d", 10+h.next)}}
	h.conts = append(h.conts, c)
}

func (h *fakeHost) Containers(_ context.Context, project, service string) ([]Container, error) {
	var out []Container
	for _, c := range h.conts {
		if c.Config.Labels[projectLabel] == project && c.Config.Labels[serviceLabel] == service {
			if !h.healthy {
				c.State.Health = &struct {
					Status string `json:"Status"`
				}{"starting"}
			}
			out = append(out, c)
		}
	}
	return out, nil
}

func (h *fakeHost) Image(_ context.Context, id string) (ImageInfo, error) {
	return ImageInfo{ID: id, RepoDigests: h.repoByID[id]}, nil
}

func (h *fakeHost) run(_ context.Context, args ...string) (string, error) {
	h.extraArgs = append(h.extraArgs, args)
	switch {
	case slices.Contains(args, "config"):
		b, _ := json.Marshal(map[string]any{"name": "shop", "services": map[string]any{"web": h.service}})
		return string(b), nil
	case slices.Contains(args, "up"):
		h.ups++
		files := []string{}
		for i, a := range args {
			if a == "--file" {
				files = append(files, args[i+1])
			}
		}
		var doc struct {
			Services map[string]struct {
				Image  string            `json:"image"`
				Labels map[string]string `json:"labels"`
			} `json:"services"`
		}
		b, err := os.ReadFile(files[len(files)-1])
		if err != nil {
			h.t.Fatal(err)
		}
		if err = json.Unmarshal(b, &doc); err != nil {
			h.t.Fatal(err)
		}
		web := doc.Services["web"]
		if web.Image == h.pullFail {
			return "", errors.New("pull access denied for " + web.Image)
		}
		h.conts = nil
		if h.partial {
			h.partial = false
			return "", errors.New("failed to create container: port is already allocated")
		}
		h.add(web.Image, web.Labels)
		return "", nil
	}
	return "", fmt.Errorf("unexpected compose args %v", args)
}

type harness struct {
	t     *testing.T
	host  *fakeHost
	b     *Backend
	m     *operation.Manager
	id    string
	token string
	now   time.Time
}

func newHarness(t *testing.T, h *fakeHost, spec operation.Spec) *harness {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")
	os.WriteFile(file, []byte("services: {}\n"), 0o600)
	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = st.InitOperations(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := &harness{t: t, host: h, now: time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)}
	x.b = &Backend{Project: "shop", Files: []string{file}, StateDir: filepath.Join(dir, "state"), Docker: h, Run: h.run}
	x.m = &operation.Manager{Store: st, Backend: x.b, Now: func() time.Time { return x.now }}
	x.m.Probe = func(_ context.Context, ip string, port int32, c operation.Check) operation.HTTPProof {
		return operation.HTTPProof{Path: c.Path, Status: 200, Passed: true, SHA256: strings.Repeat("c", 64), Bytes: 4}
	}
	if spec.Namespace == "" {
		spec.Namespace = "shop"
	}
	if spec.Name == "" {
		spec.Name = "web"
	}
	if spec.Checks == nil {
		spec.Checks = []operation.Check{{Path: "/"}}
	}
	spec.Expires = x.now.Add(time.Hour)
	r, token, err := x.m.Create(spec)
	if err != nil {
		t.Fatal(err)
	}
	x.id, x.token = r.Operation, token
	return x
}

func (x *harness) act(admin bool, q operation.Request) (operation.Result, error) {
	return x.m.Act(context.Background(), x.id, x.token, admin, q)
}

func (x *harness) call(admin bool, q operation.Request) operation.Result {
	x.t.Helper()
	r, err := x.act(admin, q)
	if err != nil {
		x.t.Fatal(err)
	}
	return r
}

func (x *harness) code(admin bool, q operation.Request, want string) {
	x.t.Helper()
	_, err := x.act(admin, q)
	var oe *operation.Error
	if !errors.As(err, &oe) || oe.Code != want {
		x.t.Fatalf("want %s, got %v", want, err)
	}
}

func (x *harness) approved(action, image string) operation.Result {
	x.t.Helper()
	p := x.call(false, operation.Request{Action: action, Image: image})
	x.call(true, operation.Request{Action: "approve", Plan: p.Plan})
	return p
}

func TestExistingComposeServiceUpdatesThroughAnApprovedPlan(t *testing.T) {
	h := newHost(t)
	h.add(tagged, nil) // started by the user with plain `docker compose up`
	h.repoByID["sha256:img-"+tagged] = []string{"nginx@sha256:" + strings.Repeat("9", 64)}
	x := newHarness(t, h, operation.Spec{Port: 80, Allow: []string{"update", "recover"}})
	v := x.call(false, operation.Request{Action: "verify"})
	if v.State != "healthy" || v.VerifiedPods != 1 {
		t.Fatal("the existing service was not verified", v)
	}
	// Models repeat facts verbatim; a Compose app has no Pods or Service.
	if facts := strings.Join(v.Facts, "\n"); !strings.Contains(facts, "passed for every new container directly and the published host port") || strings.Contains(facts, "Pod") || strings.Contains(facts, "Service") {
		t.Fatal("verify facts must describe what Compose checked", v.Facts)
	}
	good := x.stored().LastGood
	if good == nil || good.Image != "nginx@sha256:"+strings.Repeat("9", 64) {
		t.Fatal("a tagged image must resolve to its repository digest for recovery", good)
	}
	p := x.approved("update", pinnedV2)
	if h.ups != 0 {
		t.Fatal("planning or approval changed the app")
	}
	r := x.call(false, operation.Request{Action: "continue", Plan: p.Plan})
	if h.ups != 1 || r.State != "healthy" || !r.Applied || r.VerifiedImage != pinnedV2 {
		t.Fatal("approved update was not applied and verified once", r, h.ups)
	}
	c := h.conts[0]
	if c.Config.Image != pinnedV2 || c.Config.Labels[operation.Marker] != p.Plan || c.Config.Labels[generationLabel] != "1" {
		t.Fatal("container is missing the plan identity", c.Config)
	}
	if args := h.extraArgs[len(h.extraArgs)-1]; !slices.Contains(args, "--no-deps") || slices.Contains(args, "--wait") {
		t.Fatal("up must change only this service and leave readiness to verify", args)
	}
	// A second continue of the same plan reports it without running compose.
	x.call(false, operation.Request{Action: "continue", Plan: p.Plan})
	if h.ups != 1 {
		t.Fatal("an applied plan ran compose again", h.ups)
	}
}

func TestComposeExecuteIsIdempotentForOnePlanHash(t *testing.T) {
	h := newHost(t)
	h.add(tagged, nil)
	x := newHarness(t, h, operation.Spec{Port: 80, Allow: []string{"update"}})
	p := x.approved("update", pinnedV2)
	plan := x.plan(p.Plan)
	rec := x.stored()
	if _, err := x.b.Execute(context.Background(), rec, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := x.b.Execute(context.Background(), rec, plan); err != nil || h.ups != 1 {
		t.Fatal("repeating the same plan hash must not recreate containers", err, h.ups)
	}
}

func TestPullFailureLeavesServiceUnchangedAndClosesPlan(t *testing.T) {
	h := newHost(t)
	h.add(pinnedV1, nil)
	x := newHarness(t, h, operation.Spec{Port: 80, Allow: []string{"update", "recover"}})
	x.call(false, operation.Request{Action: "verify"})
	h.pullFail = "nginx:does-not-exist"
	p := x.approved("update", "nginx:does-not-exist")
	before := h.conts[0].ID
	x.code(false, operation.Request{Action: "continue", Plan: p.Plan}, "EXECUTION_FAILED")
	if h.conts[0].ID != before || x.stored().Pending != "" || !x.plan(p.Plan).Failed {
		t.Fatal("a pull failure must keep the container and close the plan", h.conts, x.stored())
	}
	d := x.call(false, operation.Request{Action: "diagnose"})
	if !strings.Contains(strings.Join(d.Facts, "\n"), "pull access denied") {
		t.Fatal("diagnose hides the failed plan", d.Facts)
	}
	rec := x.approved("recover", "")
	if rec.Review == nil || rec.Review.Image != pinnedV1 {
		t.Fatal("recovery must use the verified image", rec)
	}
	if r := x.call(false, operation.Request{Action: "continue", Plan: rec.Plan}); r.State != "healthy" || h.conts[0].Config.Image != pinnedV1 {
		t.Fatal("recovery did not restore a verified release", r)
	}
}

func TestPartialFailureStaysStartedUntilRetried(t *testing.T) {
	h := newHost(t)
	h.add(pinnedV1, nil)
	x := newHarness(t, h, operation.Spec{Port: 80, Allow: []string{"update"}})
	p := x.approved("update", pinnedV2)
	h.partial = true
	x.code(false, operation.Request{Action: "continue", Plan: p.Plan}, "EXECUTION_FAILED")
	if pl := x.plan(p.Plan); pl.Failed || pl.Started == nil || x.stored().Pending != p.Plan {
		t.Fatal("a removed container is a partial change, not an unchanged failure", pl)
	}
	x.now = x.now.Add(30 * time.Minute) // past plan expiry
	if r := x.call(false, operation.Request{Action: "continue", Plan: p.Plan}); r.State != "healthy" || h.ups != 2 {
		t.Fatal("retry did not finish the started plan", r, h.ups)
	}
}

func TestHandChangesAreReportedAsStale(t *testing.T) {
	h := newHost(t)
	h.add(pinnedV1, nil)
	x := newHarness(t, h, operation.Spec{Port: 80, Allow: []string{"update"}})
	p := x.approved("update", pinnedV2)
	x.call(false, operation.Request{Action: "continue", Plan: p.Plan})
	// Someone ran `docker compose up` without the override.
	h.conts = nil
	h.add(pinnedV1, nil)
	if v := x.call(false, operation.Request{Action: "verify"}); v.State != "stale" {
		t.Fatal("a hand-made release must not verify as this operation's goal", v)
	}
	x.code(false, operation.Request{Action: "update", Image: pinnedV1}, "STALE")
}

func TestComposeFileChangeInvalidatesAnApprovedPlan(t *testing.T) {
	h := newHost(t)
	h.add(pinnedV1, nil)
	x := newHarness(t, h, operation.Spec{Port: 80, Allow: []string{"update"}})
	p := x.approved("update", pinnedV2)
	h.service["environment"] = map[string]string{"MODE": "debug"}
	os.Chtimes(x.b.Files[0], time.Now(), time.Now().Add(time.Minute)) // invalidate the config cache
	x.code(false, operation.Request{Action: "continue", Plan: p.Plan}, "EXECUTION_FAILED")
	if h.ups != 0 || !x.plan(p.Plan).Failed {
		t.Fatal("a changed compose file must stop the approved plan before compose runs", h.ups)
	}
}

func TestUnpublishedServiceChecksEveryContainerOnly(t *testing.T) {
	h := newHost(t)
	h.service["ports"] = nil
	h.service["deploy"] = map[string]any{"replicas": 2}
	h.add(pinnedV1, nil)
	h.add(pinnedV1, nil)
	x := newHarness(t, h, operation.Spec{Port: 8080, Allow: []string{"update"}})
	var probed []string
	x.m.Probe = func(_ context.Context, ip string, port int32, c operation.Check) operation.HTTPProof {
		probed = append(probed, fmt.Sprintf("%s:%d", ip, port))
		return operation.HTTPProof{Path: c.Path, Status: 200, Passed: true}
	}
	v := x.call(false, operation.Request{Action: "verify"})
	if v.State != "healthy" || v.VerifiedPods != 2 || len(probed) != 2 || !strings.HasSuffix(probed[0], ":8080") {
		t.Fatal("expected two direct container checks and no front check", v, probed)
	}
	if !strings.Contains(fmt.Sprint(v.HTTP), "every new container directly") || strings.Contains(fmt.Sprint(v.HTTP), "published") {
		t.Fatal("HTTP scope must say what was checked", v.HTTP)
	}
	if facts := strings.Join(v.Facts, "\n"); !strings.Contains(facts, "passed for every new container directly") || strings.Contains(facts, "published") {
		t.Fatal("verify facts must not claim a host port check that did not run", v.Facts)
	}
}

func TestStartingContainersAreNotReady(t *testing.T) {
	h := newHost(t)
	h.add(pinnedV1, nil)
	h.healthy = false
	x := newHarness(t, h, operation.Spec{Port: 80, Allow: []string{"update"}})
	if v := x.call(false, operation.Request{Action: "verify"}); v.State != "not_met" {
		t.Fatal("a container whose healthcheck is starting must not be probed", v)
	}
}

func TestRouteRules(t *testing.T) {
	b := &Backend{Project: "shop"}
	s := service{Ports: []port{{Target: 80, Published: "18080", HostIP: "0.0.0.0"}, {Target: 9090}}}
	if _, err := b.route(s, "web", 0); err == nil {
		t.Fatal("two TCP ports need an explicit choice")
	}
	r, err := b.route(s, "web", 80)
	if err != nil || r.IP != "127.0.0.1" || r.Port.Port != 18080 || r.Port.TargetPort.Int != 80 {
		t.Fatal("published port did not bind to loopback", r, err)
	}
	if _, err = b.route(service{Ports: []port{{Target: 80, Published: "8000-8010"}}}, "web", 80); err == nil {
		t.Fatal("port ranges are unsupported")
	}
	var p port
	if err = json.Unmarshal([]byte(`{"target":80,"published":18080}`), &p); err != nil || p.Published != "18080" {
		t.Fatal("numeric published port", p, err)
	}
}

func (x *harness) stored() operation.Record {
	x.t.Helper()
	r, err := x.act(true, operation.Request{Action: "evidence"})
	if err != nil {
		x.t.Fatal(err)
	}
	var rec operation.Record
	x.m.Store.Write(false, func(tx *store.Tx) error {
		b, e := tx.Operation(r.Operation)
		if e == nil {
			e = json.Unmarshal(b, &rec)
		}
		return e
	})
	return rec
}

func (x *harness) plan(hash string) operation.Plan {
	x.t.Helper()
	for _, p := range x.stored().Plans {
		if p.Hash == hash {
			return p
		}
	}
	x.t.Fatal("plan not found")
	return operation.Plan{}
}

func TestComposeAppLeaseAndAutomaticRecovery(t *testing.T) {
	h := newHost(t)
	h.add(pinnedV1, nil)
	x := newHarness(t, h, operation.Spec{Port: 80, Allow: []string{"update", "recover"}, AutoRecover: true})
	spec := x.stored().Spec
	other, token, err := x.m.Create(spec)
	if err != nil {
		t.Fatal(err)
	}
	x.call(false, operation.Request{Action: "verify"})
	update := x.approved("update", pinnedV2)
	if update.Lease == nil || update.Lease.Holder != "task:"+x.id {
		t.Fatal("missing Compose lease", update)
	}
	_, err = x.m.Act(context.Background(), other.Operation, token, false, operation.Request{Action: "update", Image: pinnedV2})
	var conflict *operation.Error
	if !errors.As(err, &conflict) || conflict.Code != "APP_LEASED" || conflict.Retry() != "safe" {
		t.Fatal("Compose did not enforce the app lease", err)
	}
	x.m.Probe = func(_ context.Context, _ string, _ int32, c operation.Check) operation.HTTPProof {
		if h.conts[0].Config.Image == pinnedV2 {
			return operation.HTTPProof{Path: c.Path, Status: 500, Reason: "unexpected_status"}
		}
		return operation.HTTPProof{Path: c.Path, Status: 200, Passed: true}
	}
	r := x.call(false, operation.Request{Action: "continue", Plan: update.Plan})
	if r.State != "auto_recovered" || r.Lease != nil || r.Recovery == nil || r.Recovery.State != "healthy" || r.Recovery.Image != pinnedV1 || h.ups != 2 {
		t.Fatal("Compose did not recover exactly once", r, h.ups)
	}
	x.m = &operation.Manager{Store: x.m.Store, Backend: x.b, Now: x.m.Now, Probe: x.m.Probe}
	r = x.call(false, operation.Request{Action: "continue", Plan: update.Plan})
	if r.State != "auto_recovered" || h.ups != 2 || !strings.Contains(strings.Join(r.Recovery.UpdateFacts, " "), "HTTP 500") {
		t.Fatal("lost outcome or repeated compose up", r)
	}
}

func TestComposeOverrideBeforeUpIsNotAnExecutionReceipt(t *testing.T) {
	h := newHost(t)
	h.add(pinnedV1, nil)
	x := newHarness(t, h, operation.Spec{Port: 80, Allow: []string{"update"}})
	p := x.approved("update", pinnedV2)
	plan := x.plan(p.Plan)
	if _, err := x.b.override(x.stored(), plan, plan.BaseGeneration+1); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r := x.call(false, operation.Request{Action: "continue", Plan: p.Plan})
		if h.ups != 1 || !r.Applied || r.VerifiedImage != pinnedV2 {
			t.Fatal("the override file proved neither application nor health", r, h.ups)
		}
	}
}
