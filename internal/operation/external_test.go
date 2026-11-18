package operation

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

// memoryApp is an external backend kept in memory. Like a Compose backend it
// changes the app outside the ledger database and marks the app with the plan
// hash it last applied, which makes Execute idempotent for that plan.
type memoryApp struct {
	mu         sync.Mutex
	exists     bool
	generation int64
	image      string
	marker     string
	ready      bool
	applies    int   // real changes, not repeated reports
	executes   int   // Execute calls
	failBefore error // returned once before changing anything
	failAfter  error // returned once after changing: a lost response
}

const memoryUID = "memory-app"

func memoryRoute() *Route {
	return &Route{Name: "web", IP: "127.0.0.1", Port: apis.ServicePort{Port: 8080, Protocol: "TCP"}}
}

func (a *memoryApp) Managed(netip.Addr) bool { return false }
func (a *memoryApp) Bind(_ *store.Tx, r *Record) (bool, error) {
	if !a.exists {
		return false, nil
	}
	r.UID, r.Generation, r.Route = memoryUID, a.generation, memoryRoute()
	if r.Spec.Image == "" {
		r.Spec.Image = a.image
	}
	return true, nil
}
func (a *memoryApp) Reserve(*store.Tx, Record) error { return nil }
func (a *memoryApp) Vacant(*store.Tx, Record) error {
	if a.exists {
		return fail("STALE", "app already exists")
	}
	return nil
}
func (a *memoryApp) Base(_ *store.Tx, r Record) (string, string, error) {
	if !a.exists || r.UID != memoryUID || r.Generation != a.generation {
		return "", "", fail("STALE", "app was changed outside this operation")
	}
	return digest([]any{a.image, a.generation}), a.image, nil
}
func (a *memoryApp) Apply(*store.Tx, *Record, Plan) error {
	return errors.New("an executor backend must never be asked to apply inside the ledger")
}
func (a *memoryApp) Execute(_ context.Context, r Record, p Plan) (Target, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.executes++
	if a.marker == p.Hash {
		return Target{UID: memoryUID, Generation: a.generation, Route: memoryRoute()}, nil
	}
	if e := a.failBefore; e != nil {
		a.failBefore = nil
		return Target{}, e
	}
	if p.Action == "deploy" {
		a.exists, a.generation = true, 1
	} else {
		if a.generation != p.BaseGeneration || digest([]any{a.image, a.generation}) != p.BaseSpec {
			return Target{}, Unchanged(fail("STALE", "app changed after approval"))
		}
		a.generation++
	}
	a.image, a.marker, a.ready = p.Image, p.Hash, false
	a.applies++
	if e := a.failAfter; e != nil {
		a.failAfter = nil
		return Target{}, e
	}
	return Target{UID: memoryUID, Generation: a.generation, Route: memoryRoute()}, nil
}
func (a *memoryApp) Observe(_ *store.Tx, r Record) (Observation, error) {
	var o Observation
	if r.Generation != a.generation {
		o.External = true
		return o, nil
	}
	o.Facts = []string{"memory app ready=" + map[bool]string{true: "yes", false: "no"}[a.ready]}
	if !a.ready {
		return o, nil
	}
	o.Ready, o.Image = true, a.image
	o.Replicas = []Replica{{UID: "replica-1", ContainerID: "container-1", ImageID: a.image, IP: "127.0.0.1", Port: 8080}}
	o.FrontIP, o.FrontPort = "127.0.0.1", 8080
	o.Fingerprint = digest([]any{r.UID, r.Generation, a.image, a.marker})
	return o, nil
}
func (a *memoryApp) Diagnose(_ *store.Tx, _ Record, result *Result) error {
	result.Facts = append(result.Facts, "memory app image="+a.image)
	return nil
}

func newExternalFixture(t *testing.T, app *memoryApp) *fixture {
	t.Helper()
	st, e := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if e != nil {
		t.Fatal(e)
	}
	if e = st.InitOperations(); e != nil {
		t.Fatal(e)
	}
	f := &fixture{t: t, m: &Manager{Store: st, Backend: app}, now: time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC)}
	f.m.Now = func() time.Time { return f.now }
	f.m.Probe = func(_ context.Context, _ string, _ int32, c Check) HTTPProof {
		return HTTPProof{Path: c.Path, Status: 200, Passed: true, SHA256: strings.Repeat("b", 64), Bytes: 5}
	}
	t.Cleanup(func() { st.Close() })
	r, token, e := f.m.Create(Spec{Name: "web", Namespace: "shop", Image: imageOne, Port: 8080, Checks: []Check{{Path: "/"}}, Allow: []string{"deploy", "update", "recover"}, Expires: f.now.Add(2 * time.Hour)})
	if e != nil {
		t.Fatal(e)
	}
	f.id, f.token = r.Operation, token
	return f
}

func (f *fixture) fails(admin bool, q Request, code string) {
	f.t.Helper()
	_, e := f.m.Act(context.Background(), f.id, f.token, admin, q)
	var oe *Error
	if !errors.As(e, &oe) || oe.Code != code {
		f.t.Fatalf("want %s, got %v", code, e)
	}
}

func (f *fixture) plan(hash string) Plan {
	f.t.Helper()
	for _, p := range f.stored().Plans {
		if p.Hash == hash {
			return p
		}
	}
	f.t.Fatal("plan not found", hash)
	return Plan{}
}

func TestExternalExecutionResumesALostResponseWithoutApplyingTwice(t *testing.T) {
	app := &memoryApp{}
	f := newExternalFixture(t, app)
	p := f.call(false, Request{Action: "deploy"})
	f.call(true, Request{Action: "approve", Plan: p.Plan})
	app.failAfter = errors.New("connection reset after compose up")
	f.fails(false, Request{Action: "continue", Plan: p.Plan}, "EXECUTION_FAILED")
	r := f.stored()
	started := f.plan(p.Plan)
	if r.Pending != p.Plan || started.Started == nil || started.Applied || !strings.Contains(started.Failure, "connection reset") || app.applies != 1 {
		t.Fatal("a lost response must leave a started, unapplied plan with its failure", r, app.applies)
	}
	if s := f.call(false, Request{Action: "status"}); !strings.Contains(strings.Join(s.Facts, " "), "did not finish") {
		t.Fatal("status hides the interrupted execution", s)
	}
	// The approval window has closed, but the execution began inside it.
	f.now = f.now.Add(time.Hour)
	done := f.call(false, Request{Action: "continue", Plan: p.Plan})
	r = f.stored()
	if !done.Applied || r.UID != memoryUID || r.Generation != 1 || r.Pending != "" || app.applies != 1 || app.executes != 2 {
		t.Fatal("resume did not record the receipt exactly once", done, r, app.applies, app.executes)
	}
	f.call(false, Request{Action: "continue", Plan: p.Plan})
	if app.executes != 2 || app.applies != 1 {
		t.Fatal("an applied plan was executed again", app.executes, app.applies)
	}
}

func TestStartedPlanIsNotDiscardedBeforeItsOutcomeIsKnown(t *testing.T) {
	app := &memoryApp{}
	f := newExternalFixture(t, app)
	p := f.call(false, Request{Action: "deploy"})
	f.call(true, Request{Action: "approve", Plan: p.Plan})
	// A server crash after recording intent and before Execute returned.
	f.write(func(tx *store.Tx) error {
		r, e := load(tx, f.id)
		if e != nil {
			return e
		}
		at := f.now
		r.Plans[len(r.Plans)-1].Started = &at
		return save(tx, r, false)
	})
	f.fails(true, Request{Action: "discard", Plan: p.Plan}, "CONFLICT")
	if r := f.call(false, Request{Action: "continue", Plan: p.Plan}); !r.Applied || app.applies != 1 {
		t.Fatal("the started plan did not finish on continue", r)
	}
	// A reported, ambiguous failure may be discarded by the operator.
	app.ready = true
	f.call(false, Request{Action: "verify"})
	u := f.call(false, Request{Action: "update", Image: imageTwo})
	f.call(true, Request{Action: "approve", Plan: u.Plan})
	app.failBefore = errors.New("daemon unavailable")
	f.fails(false, Request{Action: "continue", Plan: u.Plan}, "EXECUTION_FAILED")
	f.call(true, Request{Action: "discard", Plan: u.Plan})
	if r := f.stored(); r.Pending != "" || app.image != imageOne {
		t.Fatal("discarding a failed execution changed the app or kept the plan", r, app.image)
	}
}

func TestUnchangedFailureClosesThePlanAndRecoveryUsesVerifiedImage(t *testing.T) {
	app := &memoryApp{}
	f := newExternalFixture(t, app)
	p := f.call(false, Request{Action: "deploy"})
	f.call(true, Request{Action: "approve", Plan: p.Plan})
	f.call(false, Request{Action: "continue", Plan: p.Plan})
	app.ready = true
	if v := f.call(false, Request{Action: "verify"}); v.State != "healthy" || f.stored().LastGood == nil {
		t.Fatal("the first release was not verified", v)
	}
	u := f.call(false, Request{Action: "update", Image: imageTwo})
	f.call(true, Request{Action: "approve", Plan: u.Plan})
	app.failBefore = Unchanged(errors.New("pull access denied for " + imageTwo))
	f.fails(false, Request{Action: "continue", Plan: u.Plan}, "EXECUTION_FAILED")
	r := f.stored()
	closed := f.plan(u.Plan)
	if r.Pending != "" || !closed.Failed || closed.Applied || r.Generation != 1 || app.applies != 1 {
		t.Fatal("an unchanged failure must close the plan without a receipt", r, closed)
	}
	s := f.call(false, Request{Action: "status"})
	if !strings.Contains(strings.Join(s.Facts, " "), "failed without changing the app") {
		t.Fatal("status hides the failed plan", s)
	}
	f.fails(false, Request{Action: "continue", Plan: u.Plan}, "EXECUTION_FAILED")
	rec := f.call(false, Request{Action: "recover"})
	if rec.Review == nil || rec.Review.Image != imageOne {
		t.Fatal("recovery did not use the verified image", rec)
	}
	f.call(true, Request{Action: "approve", Plan: rec.Plan})
	if r := f.call(false, Request{Action: "continue", Plan: rec.Plan}); !r.Applied || app.image != imageOne || app.applies != 2 {
		t.Fatal("recovery was not applied once", r, app.image, app.applies)
	}
}
