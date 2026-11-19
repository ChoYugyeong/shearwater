package operation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

const imageOne = "example/web@sha256:1111111111111111111111111111111111111111111111111111111111111111"
const imageTwo = "example/web@sha256:2222222222222222222222222222222222222222222222222222222222222222"

type fixture struct {
	t               *testing.T
	m               *Manager
	path, id, token string
	now             time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, e := store.Open(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = st.InitOperations(); e != nil {
		t.Fatal(e)
	}
	f := &fixture{t: t, m: &Manager{Store: st}, path: path, now: time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)}
	f.m.Now = func() time.Time { return f.now }
	t.Cleanup(func() { f.m.Store.Close() })
	f.write(func(tx *store.Tx) error {
		ns := &apis.Namespace{Metadata: apis.ObjectMeta{Name: "default"}}
		res := apis.Lookup("", "v1", "namespaces")
		objects.Stamp(res, ns)
		_, e := tx.Create(objects.Put(res, ns))
		return e
	})
	r, token, e := f.m.Create(Spec{Name: "web", Namespace: "default", Image: imageOne, Port: 8080, Checks: []Check{{Path: "/", Contains: "release"}}, Allow: []string{"deploy", "update", "recover"}, Expires: f.now.Add(time.Hour)})
	if e != nil {
		t.Fatal(e)
	}
	f.id, f.token = r.Operation, token
	f.m.Probe = func(_ context.Context, _ string, _ int32, c Check) HTTPProof {
		return HTTPProof{Path: c.Path, Status: 200, Passed: true, SHA256: strings.Repeat("a", 64), Bytes: 7}
	}
	return f
}
func (f *fixture) write(fn func(*store.Tx) error) {
	f.t.Helper()
	if e := f.m.Store.Write(false, fn); e != nil {
		f.t.Fatal(e)
	}
}
func (f *fixture) call(admin bool, q Request) Result {
	f.t.Helper()
	r, e := f.m.Act(context.Background(), f.id, f.token, admin, q)
	if e != nil {
		f.t.Fatal(e)
	}
	return r
}
func (f *fixture) reject(token string, q Request, code string) {
	f.t.Helper()
	_, e := f.m.Act(context.Background(), f.id, token, false, q)
	var oe *Error
	if !errors.As(e, &oe) || oe.Code != code {
		f.t.Fatalf("want %s, got %v", code, e)
	}
}
func (f *fixture) deploy() {
	f.t.Helper()
	p := f.call(false, Request{Action: "deploy"})
	f.call(true, Request{Action: "approve", Plan: p.Plan})
	r := f.call(false, Request{Action: "continue", Plan: p.Plan})
	if !r.Applied {
		f.t.Fatal(r)
	}
}
func (f *fixture) stored() Record {
	f.t.Helper()
	var r Record
	f.write(func(tx *store.Tx) error { var e error; r, e = load(tx, f.id); return e })
	return r
}

// A deterministic node/controller fixture. The production code still reads
// real SQLite objects and ownership links; only runtime startup and HTTP I/O
// are simulated in these unit tests. Actual containers are tested separately.
func (f *fixture) ready(suffix string) {
	f.t.Helper()
	r := f.stored()
	f.write(func(tx *store.Tx) error {
		d, rec, e := current(tx, r)
		if e != nil {
			return e
		}
		d.Status = apis.DeploymentStatus{ObservedGeneration: d.Metadata.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
		if _, _, e = tx.Update(objects.Put(apis.Lookup("apps", "v1", "deployments"), &d), rec.Rev); e != nil {
			return e
		}
		truth := true
		rs := &apis.ReplicaSet{Metadata: apis.ObjectMeta{Name: "web-" + suffix, Namespace: "default", OwnerReferences: []apis.OwnerReference{{Kind: "Deployment", Name: "web", UID: r.UID, Controller: &truth}}}, Spec: apis.ReplicaSetSpec{Replicas: d.Spec.Replicas, Selector: d.Spec.Selector, Template: d.Spec.Template}}
		rres := apis.Lookup("apps", "v1", "replicasets")
		objects.Stamp(rres, rs)
		if _, e = tx.Create(objects.Put(rres, rs)); e != nil {
			return e
		}
		p := &apis.Pod{Metadata: apis.ObjectMeta{Name: "web-" + suffix, Namespace: "default", Labels: d.Spec.Template.Metadata.Labels, Annotations: d.Spec.Template.Metadata.Annotations, OwnerReferences: []apis.OwnerReference{{Kind: "ReplicaSet", Name: rs.Metadata.Name, UID: rs.Metadata.UID, Controller: &truth}}}, Spec: d.Spec.Template.Spec}
		pres := apis.Lookup("", "v1", "pods")
		objects.Stamp(pres, p)
		p.Status = apis.PodStatus{Phase: "Running", PodIP: "10.244.0.10", Conditions: []apis.PodCondition{{Type: "Ready", Status: "True"}}, ContainerStatuses: []apis.ContainerStatus{{Name: "app", Ready: true, ContainerID: "container-" + suffix, ImageID: d.Spec.Template.Spec.Containers[0].Image, Image: d.Spec.Template.Spec.Containers[0].Image, State: apis.ContainerState{Running: &apis.ContainerStateRunning{}}}}}
		if _, e = tx.Create(objects.Put(pres, p)); e != nil {
			return e
		}
		port := int32(8080)
		protocol := "TCP"
		name := ""
		sl := &apis.EndpointSlice{Metadata: apis.ObjectMeta{Name: "web-" + suffix, Namespace: "default", Annotations: map[string]string{apis.ServiceGenerationAnnotation: "1"}, OwnerReferences: []apis.OwnerReference{{Kind: "Service", Name: r.Route.Name, UID: r.Route.UID, Controller: &truth}}}, AddressType: "IPv4", Ports: []apis.EndpointPort{{Name: &name, Protocol: &protocol, Port: &port}}, Endpoints: []apis.Endpoint{{Addresses: []string{p.Status.PodIP}, TargetRef: &apis.ObjectReference{Kind: "Pod", Namespace: "default", Name: p.Metadata.Name, UID: p.Metadata.UID}, Conditions: apis.EndpointConditions{Ready: &truth}}}}
		sres := apis.Lookup("discovery.k8s.io", "v1", "endpointslices")
		objects.Stamp(sres, sl)
		_, e = tx.Create(objects.Put(sres, sl))
		return e
	})
}
func (f *fixture) removeEndpoints(name string) {
	f.t.Helper()
	f.write(func(tx *store.Tx) error {
		k := store.Key{Group: "discovery.k8s.io", Resource: "endpointslices", Namespace: "default", Name: name}
		r, e := tx.Get(k)
		if e != nil {
			return e
		}
		_, e = tx.Delete(k, r.Rev, func(int64) ([]byte, error) { return r.Data, nil })
		return e
	})
}

func TestDeployMayRepeatButNeverReplaceTheSelectedImage(t *testing.T) {
	f := newFixture(t)
	f.reject(f.token, Request{Action: "deploy", Image: imageTwo}, "CONFLICT")
	if r := f.stored(); r.Pending != "" || len(r.Plans) != 0 || r.UID != "" {
		t.Fatal("conflicting image changed the delegated task", r)
	}
	f.reject("swop_fake", Request{Action: "deploy", Image: imageOne}, "FORBIDDEN")
	p := f.call(false, Request{Action: "deploy", Image: imageOne})
	if p.State != "awaiting_approval" || p.Review == nil || p.Review.Image != imageOne || p.Approved || p.Applied {
		t.Fatal("redundant image bypassed the normal approval boundary", p)
	}
	f.reject(f.token, Request{Action: "continue", Plan: p.Plan}, "APPROVAL_REQUIRED")
	f.call(true, Request{Action: "approve", Plan: p.Plan})
	r := f.call(false, Request{Action: "continue", Plan: p.Plan})
	if !r.Applied || f.stored().Plans[0].Image != imageOne {
		t.Fatal("the operator-selected image was not applied", r)
	}
}

func TestDelegationScopeApprovalExpiryAndRotation(t *testing.T) {
	f := newFixture(t)
	f.reject("swop_fake", Request{Action: "status"}, "FORBIDDEN")
	for _, action := range []string{"approve", "revoke", "stage", "discard"} {
		f.reject(f.token, Request{Action: action}, "FORBIDDEN")
	}
	p := f.call(false, Request{Action: "deploy"})
	f.reject(f.token, Request{Action: "continue", Plan: p.Plan}, "APPROVAL_REQUIRED")
	f.call(true, Request{Action: "approve", Plan: p.Plan})
	f.now = f.now.Add(16 * time.Minute)
	f.reject(f.token, Request{Action: "continue", Plan: p.Plan}, "EXPIRED")
	f.call(true, Request{Action: "discard", Plan: p.Plan})
	old := f.token
	_, token, e := f.m.Renew(f.id, f.now.Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	f.token = token
	f.reject(old, Request{Action: "status"}, "FORBIDDEN")
	f.call(true, Request{Action: "revoke"})
	f.reject(token, Request{Action: "status"}, "FORBIDDEN")
	_, token, e = f.m.Renew(f.id, f.now.Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	f.token = token
	f.now = f.now.Add(time.Hour)
	f.reject(token, Request{Action: "deploy"}, "FORBIDDEN")
}

func TestRecoveryMayRepeatButNeverReplaceTheVerifiedImage(t *testing.T) {
	f := newFixture(t)
	f.deploy()
	f.reject(f.token, Request{Action: "recover", Image: imageOne}, "NO_VERIFIED_IMAGE")
	f.ready("one")
	if r := f.call(false, Request{Action: "verify"}); r.State != "healthy" {
		t.Fatal(r)
	}
	f.reject(f.token, Request{Action: "recover", Image: imageTwo}, "CONFLICT")
	if r := f.stored(); r.Pending != "" || len(r.Plans) != 1 {
		t.Fatal("conflicting recovery input created a plan", r)
	}
	p := f.call(false, Request{Action: "recover", Image: imageOne})
	if p.State != "awaiting_approval" || p.Review == nil || p.Review.Image != imageOne || p.Applied || p.Approved {
		t.Fatal("recovery did not retain the independently verified image and approval", p)
	}
}

func TestContinueCannotClaimALaterPlansVerification(t *testing.T) {
	hash := strings.Repeat("a", 64)
	applied := Result{UID: "app-one", Generation: 2, Plan: hash, Applied: true, Approved: true, HTTP: "not_checked"}
	proof := Result{UID: "app-one", Generation: 2, LastPlan: hash, State: "healthy", ProofID: strings.Repeat("b", 64)}
	if r := ContinuationResult(applied, proof); r.State != "healthy" || r.Plan != hash || r.ProofID != proof.ProofID || !r.Applied {
		t.Fatal("matching verification was lost", r)
	}
	for _, changed := range []Result{
		{UID: "app-one", Generation: 3, LastPlan: strings.Repeat("c", 64), State: "healthy", ProofID: proof.ProofID},
		{UID: "replacement-app", Generation: 2, LastPlan: hash, State: "healthy", ProofID: proof.ProofID},
		{UID: "app-one", Generation: 2, LastPlan: hash, State: "awaiting_approval", Review: &Plan{Hash: strings.Repeat("c", 64)}},
	} {
		r := ContinuationResult(applied, changed)
		if r.State != "superseded" || r.Plan != hash || r.Generation != 2 || r.ProofID != "" || r.Review != nil || !r.Applied {
			t.Fatal("old request took a later goal or proof", r)
		}
	}
}

func TestOperationDoesNotAcceptAnotherGrantOrPlan(t *testing.T) {
	f := newFixture(t)
	r, token, e := f.m.Create(Spec{Name: "other", Namespace: "default", Image: imageOne, Port: 8080, Checks: []Check{{Path: "/"}}, Allow: []string{"deploy"}, Expires: f.now.Add(time.Hour)})
	if e != nil {
		t.Fatal(e)
	}
	f.reject(token, Request{Action: "status"}, "FORBIDDEN")
	p, e := f.m.Act(context.Background(), r.Operation, token, false, Request{Action: "deploy"})
	if e != nil {
		t.Fatal(e)
	}
	f.reject(f.token, Request{Action: "continue", Plan: p.Plan}, "FORBIDDEN")
	f.deploy()
	restricted, readToken, e := f.m.Create(Spec{Name: "web", Namespace: "default", Checks: []Check{{Path: "/"}}, Expires: f.now.Add(time.Hour)})
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.m.Act(context.Background(), restricted.Operation, readToken, false, Request{Action: "update", Image: imageTwo})
	var oe *Error
	if !errors.As(e, &oe) || oe.Code != "FORBIDDEN" {
		t.Fatal(e)
	}
}

func TestCommitAndReceiptAreAtomicAcrossRetryAndServerRestart(t *testing.T) {
	f := newFixture(t)
	p := f.call(false, Request{Action: "deploy"})
	f.call(true, Request{Action: "approve", Plan: p.Plan})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := f.m.Act(context.Background(), f.id, f.token, false, Request{Action: "continue", Plan: p.Plan})
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	before := f.stored()
	if before.Generation != 1 || !before.Plans[0].Applied || before.Pending != "" {
		t.Fatal(before)
	}
	if e := f.m.Store.Close(); e != nil {
		t.Fatal(e)
	}
	st, e := store.Open(f.path)
	if e != nil {
		t.Fatal(e)
	}
	f.m.Store = st
	rev, _ := st.Rev()
	r := f.call(false, Request{Action: "continue", Plan: p.Plan})
	after, _ := st.Rev()
	if !r.Applied || r.Generation != 1 || after != rev {
		t.Fatalf("retry changed state: %+v %d/%d", r, rev, after)
	}
	q := f.call(false, Request{Action: "update", Image: imageTwo})
	f.call(true, Request{Action: "approve", Plan: q.Plan})
	// A delayed duplicate of the old call must not execute the NEW approved plan.
	oldReply := f.call(false, Request{Action: "continue", Plan: p.Plan})
	if oldReply.State != "superseded" || oldReply.Review != nil {
		t.Fatal("old reply mixed a new plan into its receipt", oldReply)
	}
	if f.stored().Generation != 1 {
		t.Fatal("old retry executed a different plan")
	}
	f.call(false, Request{Action: "continue", Plan: q.Plan})
	if f.stored().Generation != 2 {
		t.Fatal("new plan was not applied")
	}
}

func TestExternalChangeInvalidatesApprovalAndPreservesUnrelatedFields(t *testing.T) {
	f := newFixture(t)
	f.deploy()
	r := f.stored()
	f.write(func(tx *store.Tx) error {
		d, rec, e := current(tx, r)
		if e != nil {
			return e
		}
		d.Spec.Template.Spec.Containers[0].Env = []apis.EnvVar{{Name: "IMPORTANT", Value: "preserve"}}
		_, _, e = tx.Update(objects.Put(apis.Lookup("apps", "v1", "deployments"), &d), rec.Rev)
		return e
	})
	p := f.call(false, Request{Action: "update", Image: imageTwo})
	f.call(true, Request{Action: "approve", Plan: p.Plan})
	f.call(false, Request{Action: "continue", Plan: p.Plan})
	r = f.stored()
	f.write(func(tx *store.Tx) error {
		d, _, e := current(tx, r)
		if e != nil {
			return e
		}
		if len(d.Spec.Template.Spec.Containers[0].Env) != 1 || d.Spec.Template.Spec.Containers[0].Env[0].Value != "preserve" {
			return fmt.Errorf("environment was not preserved")
		}
		return nil
	})
	q := f.call(false, Request{Action: "update", Image: imageOne})
	f.call(true, Request{Action: "approve", Plan: q.Plan})
	f.write(func(tx *store.Tx) error {
		d, rec, e := current(tx, r)
		if e != nil {
			return e
		}
		d.Spec.Template.Spec.Containers[0].Env[0].Value = "external change"
		d.Metadata.Generation++
		_, _, e = tx.Update(objects.Put(apis.Lookup("apps", "v1", "deployments"), &d), rec.Rev)
		return e
	})
	f.reject(f.token, Request{Action: "continue", Plan: q.Plan}, "STALE")
	if f.stored().Plans[2].Applied {
		t.Fatal("stale plan applied")
	}
}

func TestOldPodHTTP200CannotProveNewReleaseAndChecksAreRequired(t *testing.T) {
	f := newFixture(t)
	f.deploy()
	f.ready("old")
	if r := f.call(false, Request{Action: "verify"}); r.State != "healthy" || r.VerifiedPods != 1 || !ValidHash(r.ProofID) || r.Evidence != nil ||
		!strings.Contains(strings.Join(r.Facts, "\n"), "passed for new-release Pods directly and bound internal Service") {
		t.Fatal(r)
	}
	p := f.call(false, Request{Action: "update", Image: imageTwo})
	f.call(true, Request{Action: "approve", Plan: p.Plan})
	f.call(false, Request{Action: "continue", Plan: p.Plan})
	if r := f.call(false, Request{Action: "verify"}); r.State == "healthy" {
		t.Fatal("old HTTP200 accepted")
	}
	f.ready("new")
	if r := f.call(false, Request{Action: "verify"}); r.State == "healthy" {
		t.Fatal("mixed old/new endpoint accepted")
	}
	f.removeEndpoints("web-old")
	f.m.Probe = func(_ context.Context, _ string, _ int32, c Check) HTTPProof {
		return HTTPProof{Path: c.Path, Status: 200, Reason: "expected_content_missing"}
	}
	if r := f.call(false, Request{Action: "verify"}); r.State != "http_failed" {
		t.Fatal(r)
	}
	if f.stored().LastGood.Image != imageOne {
		t.Fatal("failed proof replaced recovery image")
	}
	q := f.call(false, Request{Action: "recover"})
	if q.Review.Image != imageOne {
		t.Fatal(q)
	}
}

func TestHealthyCurrentImageDoesNotCompleteStagedUpdate(t *testing.T) {
	f := newFixture(t)
	f.deploy()
	f.ready("current")
	f.call(true, Request{Action: "stage", Image: imageTwo})
	r := f.call(false, Request{Action: "verify"})
	if r.State != "healthy" || r.VerifiedImage != imageOne || r.StagedImage != imageTwo || r.Plan != "" {
		t.Fatal(r)
	}
	if !strings.Contains(r.Next, "NOT planned or deployed") || !strings.Contains(r.Next, "call update") {
		t.Fatal("current health misrepresented staged update completion", r.Next)
	}
	if f.stored().LastGood.Image != imageOne {
		t.Fatal("staged input became recovery evidence without being deployed")
	}
}

func TestProbeRechecksContainerAndRevocationBeforeSavingProof(t *testing.T) {
	for _, mode := range []string{"container", "revoke", "expire"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.deploy()
			f.ready("one")
			once := sync.Once{}
			f.m.Probe = func(_ context.Context, _ string, _ int32, c Check) HTTPProof {
				once.Do(func() {
					switch mode {
					case "revoke":
						f.call(true, Request{Action: "revoke"})
					case "expire":
						f.now = f.now.Add(2 * time.Hour)
					case "container":
						f.write(func(tx *store.Tx) error {
							rec, e := tx.Get(key("pods", "default", "web-one"))
							if e != nil {
								return e
							}
							var p apis.Pod
							if e = decode(rec, &p); e != nil {
								return e
							}
							p.Status.ContainerStatuses[0].ContainerID = "replacement"
							_, _, e = tx.Update(objects.Put(apis.Lookup("", "v1", "pods"), &p), rec.Rev)
							return e
						})
					}
				})
				return HTTPProof{Path: c.Path, Status: 200, Passed: true}
			}
			r, e := f.m.Act(context.Background(), f.id, f.token, false, Request{Action: "verify"})
			if mode == "container" {
				if e != nil || r.State != "stale" {
					t.Fatalf("%+v %v", r, e)
				}
			} else {
				var oe *Error
				if !errors.As(e, &oe) || oe.Code != "FORBIDDEN" {
					t.Fatal(e)
				}
			}
			if f.stored().LastGood != nil {
				t.Fatal("stale/unauthorized proof was saved")
			}
		})
	}
}

func TestOperationRecordContainsNoTokenAndResultNoEnvironment(t *testing.T) {
	if matches(map[string]string{"required": ""}, map[string]string{}) {
		t.Fatal("missing selector key matched an empty value")
	}
	f := newFixture(t)
	f.deploy()
	r := f.stored()
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), f.token) {
		t.Fatal("raw token stored")
	}
	got := f.call(false, Request{Action: "diagnose"})
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "tokenHash") || strings.Contains(string(b), "env") {
		t.Fatal(string(b))
	}
	if len(got.Facts) == 0 || len(got.Unknown) == 0 {
		t.Fatal(fmt.Sprint(got))
	}
}
