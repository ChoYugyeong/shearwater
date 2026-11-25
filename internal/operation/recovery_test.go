package operation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func enableRecovery(t *testing.T, f *fixture) {
	t.Helper()
	spec := f.stored().Spec
	spec.AutoRecover = true
	grant, token, err := f.m.Create(spec)
	if err != nil {
		t.Fatal(err)
	}
	f.id, f.token = grant.Operation, token
	if !f.call(true, Request{Action: "review"}).AutoRecover {
		t.Fatal("review hides pre-authorization")
	}
}

func failedProbe(_ context.Context, _ string, _ int32, c Check) HTTPProof {
	return HTTPProof{Path: c.Path, Status: 503, Reason: "unexpected_status"}
}

func updateFixture(f *fixture) string {
	f.t.Helper()
	p := f.call(false, Request{Action: "update", Image: imageTwo})
	f.call(true, Request{Action: "approve", Plan: p.Plan})
	r := f.call(false, Request{Action: "continue", Plan: p.Plan})
	if r.State != "not_met" || r.Recovery != nil {
		f.t.Fatal("starting rollout triggered a recovery", r)
	}
	return p.Plan
}

func TestAutoRecoveryRequiresDelegatedRecovery(t *testing.T) {
	f := newFixture(t)
	spec := f.stored().Spec
	spec.Allow, spec.AutoRecover = []string{"deploy", "update"}, true
	_, _, err := f.m.Create(spec)
	var rejected *Error
	if !errors.As(err, &rejected) || rejected.Code != "INVALID" {
		t.Fatal(err)
	}
}

func TestAutoRecoveryWithoutOptInOrVerifiedImage(t *testing.T) {
	for _, opted := range []bool{false, true} {
		f := newFixture(t)
		if opted {
			enableRecovery(t, f)
		}
		f.deploy()
		if !opted {
			f.ready("old")
			f.call(false, Request{Action: "verify"})
			f.removeEndpoints("web-old")
		}
		updateFixture(f)
		f.ready("broken")
		f.m.Probe = failedProbe
		r := f.call(false, Request{Action: "verify"})
		if r.State != "http_failed" || len(f.stored().Plans) != 2 || r.Lease != nil {
			t.Fatal("unexpected recovery", r)
		}
		if opted {
			if r.Recovery == nil || r.Recovery.State != "skipped" || !strings.Contains(strings.Join(r.Recovery.Facts, " "), "no previously HTTP-verified") {
				t.Fatal("missing reason for skipped recovery", r)
			}
		} else if r.AutoRecover || r.Recovery != nil {
			t.Fatal("auto-recovery should be off by default", r)
		}
	}
}

func TestAutoRecoveryRetainsBothOutcomesAcrossRestart(t *testing.T) {
	for _, recoveryPasses := range []bool{true, false} {
		f := newFixture(t)
		enableRecovery(t, f)
		f.deploy()
		f.ready("good")
		f.call(false, Request{Action: "verify"})
		f.call(true, Request{Action: "stage", Image: imageTwo, Checks: []Check{{Path: "/release-two"}}})
		parent := updateFixture(f)
		f.removeEndpoints("web-good")
		f.ready("bad")
		passingProbe := f.m.Probe
		f.m.Probe = failedProbe
		start := f.call(false, Request{Action: "continue", Plan: parent})
		report := start.Recovery
		if report == nil || report.UpdatePlan != parent || report.Image != imageOne || report.State != "verifying" || start.State != "not_met" || start.Lease == nil {
			t.Fatal("failure did not transfer the lease to a recovery", start)
		}
		if child := f.plan(report.Plan); len(child.Checks) != 1 || child.Checks[0].Path != "/" {
			t.Fatal("recovery did not use the verified checks", child.Checks)
		}
		if !ValidHash(report.UpdateProof) || !strings.Contains(strings.Join(report.UpdateFacts, " "), "HTTP 503") || f.plan(report.Plan).AutoRecoverOf != parent {
			t.Fatal("lost failure evidence or plan link", report)
		}
		if e := f.m.Store.Close(); e != nil {
			t.Fatal(e)
		}
		st, err := store.Open(f.path)
		if err != nil {
			t.Fatal(err)
		}
		f.m = &Manager{Store: st, Now: f.m.Now, Probe: failedProbe}
		if recoveryPasses {
			f.m.Probe = passingProbe
		}
		f.removeEndpoints("web-bad")
		f.ready("restored")
		want, proof := "auto_recovery_failed", "http_failed"
		if recoveryPasses {
			want, proof = "auto_recovered", "healthy"
		}
		for _, hash := range []string{parent, report.Plan, parent} {
			r := f.call(false, Request{Action: "continue", Plan: hash})
			if r.State != want || r.Recovery == nil || r.Recovery.State != proof || r.Recovery.Plan != report.Plan || r.Lease != nil {
				t.Fatal("wrong replay result", r)
			}
		}
		rec := f.stored()
		if rec.Generation != 3 || len(rec.Plans) != 3 || rec.Pending != "" {
			t.Fatal("duplicate recovery application or decision")
		}
		ev := f.call(true, Request{Action: "evidence"})
		if ev.Evidence == nil || ev.Evidence.Generation != 3 || ev.Evidence.Image != imageOne || ev.Recovery.UpdateProof != report.UpdateProof || !ValidHash(ev.Recovery.ProofID) {
			t.Fatal("evidence lost one of the outcomes", ev)
		}
	}
}

func TestAutoRecoveryAfterTerminalRolloutFailure(t *testing.T) {
	f := newFixture(t)
	enableRecovery(t, f)
	f.deploy()
	f.ready("good")
	f.call(false, Request{Action: "verify"})
	parent := updateFixture(f)
	r := f.stored()
	f.write(func(tx *store.Tx) error {
		d, rec, err := current(tx, r)
		if err != nil {
			return err
		}
		d.Status.ObservedGeneration = r.Generation
		d.Status.Conditions = []apis.DeploymentCondition{{Type: "Progressing", Status: "False", Reason: "ProgressDeadlineExceeded"}}
		_, _, err = tx.Update(objects.Put(apis.Lookup("apps", "v1", "deployments"), &d), rec.Rev)
		return err
	})
	got := f.call(false, Request{Action: "verify"})
	if got.Recovery == nil || got.Recovery.UpdateState != "rollout_failed" || got.Recovery.UpdatePlan != parent || got.Recovery.Image != imageOne {
		t.Fatal("missing terminal rollout evidence", got)
	}
}

func TestAutoRecoveryResumesInterruptedExecutor(t *testing.T) {
	app := &memoryApp{exists: true, generation: 1, image: imageOne, ready: true}
	f := newExternalFixture(t, app)
	enableRecovery(t, f)
	f.call(false, Request{Action: "verify"})
	parent := updateFixture(f)
	app.ready = true
	f.m.Probe = failedProbe
	app.failAfter = errors.New("lost response after applying recovery")
	got := f.call(false, Request{Action: "verify"})
	if got.State != "auto_recovery_interrupted" || got.Recovery == nil || got.Recovery.UpdatePlan != parent || app.applies != 2 {
		t.Fatal("missing interrupted recovery receipt", got)
	}
	child := got.Recovery.Plan
	if r := f.stored(); r.Pending != child || r.Plans[1].Applied || r.Plans[1].Started == nil {
		t.Fatal("recovery intent was not durable", r)
	}
	f.m = &Manager{Store: f.m.Store, Backend: app, Now: f.m.Now, Probe: failedProbe}
	app.ready = true
	got = f.call(false, Request{Action: "continue", Plan: parent})
	if got.State != "auto_recovery_failed" || got.Recovery.Plan != child || got.Recovery.State != "http_failed" || app.applies != 2 || f.stored().Pending != "" {
		t.Fatal("lost-response recovery executed twice or recursed", got, app.applies)
	}
}

func TestExpiredUpdateCannotRecoverOverAnotherHolder(t *testing.T) {
	f := newFixture(t)
	enableRecovery(t, f)
	f.deploy()
	f.ready("one")
	f.call(false, Request{Action: "verify"})
	parent := updateFixture(f)
	f.removeEndpoints("web-one")
	f.ready("two")
	f.now = f.plan(parent).Expires
	other, token, err := f.m.Create(f.stored().Spec)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.m.Act(context.Background(), other.Operation, token, false, Request{Action: "update", Image: "web:other"})
	if err != nil {
		t.Fatal(err)
	}
	f.m.Probe = failedProbe
	got := f.call(false, Request{Action: "verify"})
	if got.Recovery == nil || got.Recovery.State != "skipped" || got.Lease == nil || got.Lease.Holder != "task:"+other.Operation || len(f.stored().Plans) != 2 {
		t.Fatal("late failure displaced a newer holder", got)
	}
}

func TestCommittedRecoveryDecisionResumesBeforeFirstExecution(t *testing.T) {
	f := newFixture(t)
	enableRecovery(t, f)
	f.deploy()
	f.ready("good")
	f.call(false, Request{Action: "verify"})
	parent := updateFixture(f)
	var child string
	// Simulate the crash exactly after committing the verification decision
	// and before dispatching its child for execution.
	f.write(func(tx *store.Tx) error {
		r, err := load(tx, f.id)
		if err != nil {
			return err
		}
		s := snapshot{record: r}
		result := Result{State: "rollout_failed", Facts: []string{"goal generation reported a terminal failure"}}
		return f.m.recordVerification(tx, &s, &result, &child)
	})
	if !ValidHash(child) || f.plan(child).Applied || f.stored().Pending != child {
		t.Fatal("missing recovery decision before restart")
	}
	if err := f.m.Store.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.m = &Manager{Store: st, Now: f.m.Now, Probe: f.m.Probe}
	f.call(false, Request{Action: "verify"})
	f.call(false, Request{Action: "continue", Plan: parent})
	if r := f.stored(); r.Generation != 3 || len(r.Plans) != 3 || !r.Plans[2].Applied || r.Plans[2].Hash != child {
		t.Fatal("restart created or applied a second recovery", r)
	}
}
