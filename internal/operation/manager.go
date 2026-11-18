package operation

import (
	"context"
	"crypto/subtle"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

func (m *Manager) authorize(r Record, token string, admin bool) error {
	if admin {
		return nil
	}
	if r.Revoked || !m.now().Before(r.Spec.Expires) || token == "" || subtle.ConstantTimeCompare([]byte(digest(token)), []byte(r.TokenHash)) != 1 {
		return fail("FORBIDDEN", "delegation is invalid, expired, or revoked")
	}
	return nil
}

func (m *Manager) Create(spec Spec) (Result, string, error) {
	var result Result
	if !apis.IsDNS1123Label(spec.Name) || !apis.IsDNS1123Label(spec.Namespace) || spec.Port < 0 || spec.Port > 65535 || spec.Service != "" && !apis.IsDNS1123Label(spec.Service) || spec.Image != "" && !validImage(spec.Image) {
		return result, "", fail("INVALID", "invalid app, namespace, service, image or port")
	}
	if e := validateChecks(spec.Checks); e != nil {
		return result, "", e
	}
	if !spec.Expires.After(m.now()) || spec.Expires.After(m.now().Add(24*time.Hour)) {
		return result, "", fail("INVALID", "delegation expiry must be within 24 hours")
	}
	if len(spec.Allow) > 3 {
		return result, "", fail("INVALID", "allow only deploy, update, recover")
	}
	seen := map[string]bool{}
	for _, a := range spec.Allow {
		if seen[a] || (a != "deploy" && a != "update" && a != "recover") {
			return result, "", fail("INVALID", "allow only unique deploy, update, recover actions")
		}
		seen[a] = true
	}
	token := "swop_" + randomID(32)
	r := Record{ID: randomID(16), TokenHash: digest(token), Spec: spec, GoalChecks: spec.Checks}
	err := m.Store.Write(false, func(tx *store.Tx) error {
		found, e := m.backend().Bind(tx, &r)
		if e != nil {
			return e
		}
		if !found {
			if !seen["deploy"] || !validImage(spec.Image) || spec.Port == 0 {
				return fail("INVALID", "a missing app needs deploy permission, image and port")
			}
			if e = m.backend().Reserve(tx, r); e != nil {
				return e
			}
		}
		if e = save(tx, r, true); e != nil {
			return e
		}
		result = summary(r)
		return nil
	})
	return result, token, err
}

// Renew rotates the secret, never the app identity or permitted actions. A new
// client/another model can resume the same server record; the old token stops
// working immediately. Only an administrator may reach this method via HTTP.
func (m *Manager) Renew(id string, expires time.Time) (Result, string, error) {
	var result Result
	if !expires.After(m.now()) || expires.After(m.now().Add(24*time.Hour)) {
		return result, "", fail("INVALID", "expiry must be within 24 hours")
	}
	token := "swop_" + randomID(32)
	e := m.Store.Write(false, func(tx *store.Tx) error {
		r, e := load(tx, id)
		if e != nil {
			return e
		}
		r.TokenHash, r.Revoked, r.Spec.Expires = digest(token), false, expires
		if e = save(tx, r, false); e != nil {
			return e
		}
		result = summary(r)
		return nil
	})
	return result, token, e
}

func summary(r Record) Result {
	state := "needs_verification"
	if r.UID == "" {
		state = "not_deployed"
	}
	result := Result{Format: "shearwater.workflow-result/v1", Operation: r.ID, App: r.Spec.Namespace + "/" + r.Spec.Name, UID: r.UID, Generation: r.Generation, State: state, Allowed: r.Spec.Allow, Expires: r.Spec.Expires, HTTP: "not_checked", Facts: []string{}, Unknown: []string{"public URL, authentication and data migrations are not checked"}, Next: "verify the selected app; status alone is not proof of health"}
	result.StagedImage = r.StagedImage
	if r.StagedImage != "" {
		result.Next = "an update image is staged, not planned or applied; if update was requested, call update to SAVE its plan before asking for approval"
		result.Unknown = append(result.Unknown, "stagedImage is a future input, never evidence of the currently running image")
	}
	for _, p := range r.Plans {
		if p.Applied {
			result.LastPlan = p.Hash
		}
	}
	if r.Pending != "" {
		for _, p := range r.Plans {
			if p.Hash != r.Pending {
				continue
			}
			result.Plan = p.Hash
			result.Approved = p.Approved
			result.Review = &p
			result.State = "awaiting_approval"
			result.Handoff = "operator_approval"
			result.Next = "operator: review this exact server plan and approve its hash; agent: stop"
			if p.Approved {
				result.State = "approved_pending"
				result.Handoff = ""
				result.Next = "continue the exact approved plan; do not create another plan"
			}
			if p.Failure != "" {
				result.Facts = append(result.Facts, "execution of the approved plan did not finish: "+p.Failure)
				result.Next = "continue retries this same plan without applying it twice; diagnose first if the failure repeats; the operator may discard it"
			}
			break
		}
	} else if len(r.Plans) > 0 {
		p := r.Plans[len(r.Plans)-1]
		result.Applied = p.Applied
		if p.Failed {
			result.Facts = append(result.Facts, "the last plan failed without changing the app: "+p.Failure)
			result.Next = "diagnose; the failed plan cannot be continued, so make a new plan if a change is still wanted"
		}
	}
	if r.Revoked {
		result.State = "revoked"
	}
	return result
}

func (m *Manager) Act(ctx context.Context, id, token string, admin bool, q Request) (Result, error) {
	var result Result
	if !ValidID(id) {
		return result, fail("INVALID", "invalid operation id")
	}
	if q.Image != "" && q.Action != "deploy" && q.Action != "update" && q.Action != "recover" && q.Action != "stage" {
		return result, fail("INVALID", "only deploy, update, recover or operator stage accepts image")
	}
	if len(q.Checks) > 0 && q.Action != "stage" {
		return result, fail("INVALID", "only operator stage sets new checks")
	}
	if q.Plan != "" && q.Action != "continue" && q.Action != "approve" && q.Action != "discard" {
		return result, fail("INVALID", "unexpected plan hash")
	}
	if q.Action == "verify" {
		return m.verify(ctx, id, token, admin)
	}
	if x, ok := m.backend().(Executor); ok && q.Action == "continue" {
		result, err := m.continueExternal(ctx, x, id, token, admin, q)
		return m.verifyApplied(ctx, id, token, admin, result, err)
	}
	err := m.Store.Write(false, func(tx *store.Tx) error {
		r, e := load(tx, id)
		if e != nil {
			return e
		}
		if e = m.authorize(r, token, admin); e != nil {
			return e
		}
		// Check permission before looking up app state. A forged action must
		// not expose another app, change a policy, or manufacture approval.
		switch q.Action {
		case "status", "diagnose", "review", "evidence":
			result = summary(r)
			if q.Action == "evidence" {
				if !admin {
					return fail("FORBIDDEN", "full retained proof is an operator action")
				}
				result.Evidence = r.LastCheck
				if r.LastCheck != nil {
					result.ProofID = digest(r.LastCheck)
				}
			}
			if q.Action == "diagnose" {
				e = m.diagnose(tx, r, &result)
			}
			return e
		case "approve", "discard", "stage", "revoke":
			if !admin {
				return fail("FORBIDDEN", "only the operator may approve, discard, stage, or revoke")
			}
		case "deploy", "update", "recover":
			if !slices.Contains(r.Spec.Allow, q.Action) {
				return fail("FORBIDDEN", "action is outside delegated scope")
			}
		case "continue":
		default:
			return fail("INVALID", "unknown operation action")
		}
		switch q.Action {
		case "deploy", "update", "recover":
			e = m.plan(tx, &r, q)
		case "continue":
			e = m.apply(tx, &r, q.Plan)
		case "approve", "discard":
			if !ValidHash(q.Plan) || r.Pending != q.Plan {
				return fail("CONFLICT", "confirm the exact pending plan hash")
			}
			p := &r.Plans[len(r.Plans)-1]
			if p.Hash != q.Plan || p.Applied {
				return fail("CONFLICT", "pending plan mismatch")
			}
			if q.Action == "discard" && p.Started != nil && p.Failure == "" {
				return fail("CONFLICT", "execution of this plan started; continue it to record the result")
			}
			if q.Action == "approve" {
				if !m.now().Before(p.Expires) {
					return fail("EXPIRED", "plan expired; discard and prepare a new one")
				}
				p.Approved = true
			} else {
				p.Discarded = true
				r.Pending = ""
			}
		case "stage":
			if r.Pending != "" {
				return fail("CONFLICT", "finish or discard pending plan first")
			}
			if !validImage(q.Image) {
				return fail("INVALID", "stage requires a bounded image")
			}
			if len(q.Checks) > 0 {
				if e = validateChecks(q.Checks); e != nil {
					return e
				}
			}
			r.StagedImage, r.StagedChecks = q.Image, q.Checks
		case "revoke":
			r.Revoked = true
		}
		if e != nil {
			return e
		}
		if e = save(tx, r, false); e != nil {
			return e
		}
		result = summary(r)
		if q.Action == "continue" {
			result = continued(r, q.Plan)
		}
		return nil
	})
	if q.Action == "continue" {
		return m.verifyApplied(ctx, id, token, admin, result, err)
	}
	return result, err
}

// continued describes the plan a continue request named, not merely the
// latest record state. A later plan or generation marks it superseded.
func continued(r Record, hash string) Result {
	result := summary(r)
	for _, p := range r.Plans {
		if p.Hash == hash {
			result.Plan = p.Hash
			result.Applied = p.Applied
			result.Approved = p.Approved
			result.Generation = p.Generation
			break
		}
	}
	if result.Generation != r.Generation || r.Pending != "" {
		result.State = "superseded"
		result.Review = nil
		result.Next = "this plan already applied; read status for later work; do not repeat it"
	}
	return result
}

// verifyApplied observes an applied plan's goal. Runtime startup/readiness is
// deliberately outside the SQL transaction. Replaying the same plan commits no
// additional app update, then observes that plan's goal. A later pending plan
// cannot be substituted for it.
func (m *Manager) verifyApplied(ctx context.Context, id, token string, admin bool, result Result, err error) (Result, error) {
	if err == nil && result.Applied && result.State != "superseded" && result.Review == nil {
		v, e := m.verify(ctx, id, token, admin)
		if e == nil {
			v = ContinuationResult(result, v)
		}
		return v, e
	}
	return result, err
}

// continueExternal executes an approved plan through an Executor in three
// steps: record that execution started, run the backend without holding the
// database, then record the receipt. A crash between the steps leaves a started
// plan that the next continue resumes; the backend's plan-hash marker keeps it
// from being applied twice.
func (m *Manager) continueExternal(ctx context.Context, x Executor, id, token string, admin bool, q Request) (Result, error) {
	var result Result
	var run *Plan
	var bound Record
	err := m.Store.Write(false, func(tx *store.Tx) error {
		r, e := load(tx, id)
		if e != nil {
			return e
		}
		if e = m.authorize(r, token, admin); e != nil {
			return e
		}
		p, e := m.admit(&r, q.Plan, true)
		if e != nil {
			return e
		}
		if p == nil {
			result = continued(r, q.Plan)
			return nil
		}
		if p.Started == nil {
			at := m.now()
			p.Started = &at
		}
		p.Failure = ""
		if e = save(tx, r, false); e != nil {
			return e
		}
		copy := *p
		run, bound = &copy, r
		return nil
	})
	if err != nil || run == nil {
		return result, err
	}
	target, failure := x.Execute(ctx, bound, *run)
	err = m.Store.Write(false, func(tx *store.Tx) error {
		r, e := load(tx, id)
		if e != nil {
			return e
		}
		var p *Plan
		for i := range r.Plans {
			if r.Plans[i].Hash == run.Hash {
				p = &r.Plans[i]
			}
		}
		if p == nil || r.Pending != run.Hash && !p.Applied {
			return fail("STALE", "plan record changed during execution")
		}
		if !p.Applied {
			var u unchanged
			switch {
			case failure == nil:
				r.UID, r.Generation, r.Route = target.UID, target.Generation, target.Route
				m.settle(&r, p)
			case errors.As(failure, &u):
				p.Failure, p.Failed = bounded(failure.Error()), true
				r.Pending = ""
			default:
				p.Failure = bounded(failure.Error())
			}
			if e = save(tx, r, false); e != nil {
				return e
			}
		}
		result = continued(r, run.Hash)
		return nil
	})
	if failure != nil {
		var u unchanged
		if errors.As(failure, &u) {
			return result, fail("EXECUTION_FAILED", "the app was left unchanged and this plan is closed: "+bounded(failure.Error()))
		}
		return result, fail("EXECUTION_FAILED", "execution did not finish; continue retries the same plan, or the operator may discard it: "+bounded(failure.Error()))
	}
	return result, err
}

// bounded keeps execution errors short and printable before they are stored
// or returned to a model.
func bounded(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}

// ContinuationResult keeps an applied receipt attached to matching observations.
// Application and HTTP observation deliberately use different transactions.
// Another client may plan/apply between them. Never attach the old plan hash
// to a newer deployment's proof (or replace a new pending plan's identity).
func ContinuationResult(applied, observed Result) Result {
	if observed.UID != applied.UID || observed.Generation != applied.Generation || observed.LastPlan != applied.Plan || observed.Review != nil {
		applied.State, applied.Review, applied.Handoff = "superseded", nil, ""
		applied.ProofID, applied.VerifiedImage, applied.VerifiedPods = "", "", 0
		applied.CheckedAt, applied.Evidence, applied.HTTP = nil, nil, "not_checked"
		applied.Next = "this plan already applied; later work appeared before verification; read status without repeating it"
		return applied
	}
	observed.Plan, observed.Applied, observed.Approved = applied.Plan, true, true
	return observed
}

func (m *Manager) plan(tx *store.Tx, r *Record, q Request) error {
	if r.Pending != "" {
		return fail("CONFLICT", "pending plan exists; continue it or ask operator to discard")
	}
	if len(r.Plans) >= 32 {
		return fail("LIMIT", "32-plan history is full; retain it and create a new operation")
	}
	p := Plan{Action: q.Action, BaseUID: r.UID, BaseGeneration: r.Generation, RouteHash: digest(r.Route), Checks: r.GoalChecks, Created: m.now(), Expires: m.now().Add(15 * time.Minute)}
	if r.Spec.Expires.Before(p.Expires) {
		p.Expires = r.Spec.Expires
	}
	if q.Action == "deploy" {
		if r.UID != "" {
			return fail("CONFLICT", "app already exists; use update")
		}
		// A shared, stable tool schema also contains the optional update image.
		// Small models may repeat the already selected image when deploying.
		// Accept that redundant value ONLY when it equals the operator's input:
		// it cannot replace the chosen image or weaken the exact-plan approval.
		if q.Image != "" && q.Image != r.Spec.Image {
			return fail("CONFLICT", "initial image is already selected; omit image")
		}
		p.Image = r.Spec.Image
		if e := m.backend().Vacant(tx, *r); e != nil {
			return e
		}
	} else {
		base, before, e := m.backend().Base(tx, *r)
		if e != nil {
			return e
		}
		p.BaseSpec, p.BeforeImage = base, before
		p.Image = q.Image
		if q.Action == "recover" {
			if r.LastGood == nil {
				return fail("NO_VERIFIED_IMAGE", "verify a healthy version before recovery")
			}
			// As for deploy, repeating the exact server-selected value is harmless.
			// A model must never override the image established by real HTTP proof.
			if q.Image != "" && q.Image != r.LastGood.Image {
				return fail("CONFLICT", "recovery image is fixed by verified evidence; omit image")
			}
			p.Image = r.LastGood.Image
			p.Checks = r.LastGood.Checks
		} else if r.StagedImage != "" {
			if q.Image != "" && q.Image != r.StagedImage {
				return fail("CONFLICT", "update image is already staged; omit image")
			}
			p.Image = r.StagedImage
			if len(r.StagedChecks) > 0 {
				p.Checks = r.StagedChecks
			}
		}
	}
	if !validImage(p.Image) {
		return fail("INVALID", "update requires an image or an operator-staged build artifact")
	}
	p.Hash = digest(struct {
		Task string
		Plan Plan
	}{r.ID, p})
	r.Pending = p.Hash
	r.Plans = append(r.Plans, p)
	return nil
}

func (m *Manager) apply(tx *store.Tx, r *Record, hash string) error {
	p, e := m.admit(r, hash, false)
	if e != nil || p == nil {
		return e // a nil plan was already applied: the transaction is the receipt
	}
	if e = m.backend().Apply(tx, r, *p); e != nil {
		return e
	}
	m.settle(r, p)
	return nil
}

// admit returns the approved pending plan a continue request may execute, or
// nil when that plan was already applied. With resume, a plan whose external
// execution already started is returned even after expiry so it can finish.
func (m *Manager) admit(r *Record, hash string, resume bool) (*Plan, error) {
	if !ValidHash(hash) {
		return nil, fail("INVALID", "continue needs the exact observed plan hash")
	}
	var p *Plan
	for i := range r.Plans {
		if r.Plans[i].Hash == hash {
			p = &r.Plans[i]
			break
		}
	}
	if p == nil {
		return nil, fail("FORBIDDEN", "plan does not belong to this operation")
	}
	if !slices.Contains(r.Spec.Allow, p.Action) {
		return nil, fail("FORBIDDEN", "plan action is not delegated")
	}
	if p.Applied {
		return nil, nil
	}
	if p.Failed {
		return nil, fail("EXECUTION_FAILED", "this plan failed without changing the app; make a new plan")
	}
	if r.Pending != hash || !p.Approved || p.Discarded {
		return nil, fail("APPROVAL_REQUIRED", "the operator must approve this exact pending plan")
	}
	if resume && p.Started != nil {
		return p, nil
	}
	if !m.now().Before(p.Expires) {
		return nil, fail("EXPIRED", "plan expired; do not replay it")
	}
	if p.BaseUID != r.UID || p.BaseGeneration != r.Generation || p.RouteHash != digest(r.Route) {
		return nil, fail("STALE", "operation target changed after planning")
	}
	return p, nil
}

// settle records an applied plan as the operation's new goal.
func (m *Manager) settle(r *Record, p *Plan) {
	p.Applied, p.Generation = true, r.Generation
	r.GoalChecks = p.Checks
	r.Pending = ""
	r.StagedImage = ""
	r.StagedChecks = nil
}
