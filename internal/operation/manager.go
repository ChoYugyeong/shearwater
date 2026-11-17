package operation

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
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
		if _, e := tx.Get(key("namespaces", "", spec.Namespace)); e != nil {
			return e
		}
		rec, e := tx.Get(key("deployments", spec.Namespace, spec.Name))
		if e == nil {
			var d apis.Deployment
			if e = decode(rec, &d); e != nil {
				return e
			}
			if e = eligible(d); e != nil {
				return e
			}
			r.UID, r.Generation = d.Metadata.UID, d.Metadata.Generation
			r.Route, e = discoverRoute(tx, r, d)
			if e != nil {
				return e
			}
			if spec.Image == "" {
				r.Spec.Image = d.Spec.Template.Spec.Containers[0].Image
			}
		} else if errors.Is(e, store.ErrNotFound) {
			if !seen["deploy"] || !validImage(spec.Image) || spec.Port == 0 {
				return fail("INVALID", "a missing app needs deploy permission, image and port")
			}
			if spec.Service != "" && spec.Service != spec.Name {
				return fail("INVALID", "a new app uses a same-named Service")
			}
			if _, e = tx.Get(key("services", spec.Namespace, spec.Name)); !errors.Is(e, store.ErrNotFound) {
				return fail("CONFLICT", "new app Service name is already present or unreadable")
			}
		} else {
			return e
		}
		if e = save(tx, r, true); e != nil {
			return e
		}
		result = summary(r)
		return nil
	})
	return result, token, err
}

func eligible(d apis.Deployment) error {
	if d.Metadata.DeletionTimestamp != nil || d.Spec.Paused || len(d.Spec.Template.Spec.Containers) != 1 || d.Spec.Replicas == nil || *d.Spec.Replicas < 1 || *d.Spec.Replicas > 16 {
		return fail("UNSUPPORTED", "operations support an active one-container Deployment with 1..16 replicas")
	}
	return nil
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
			break
		}
	} else if len(r.Plans) > 0 {
		p := r.Plans[len(r.Plans)-1]
		result.Applied = p.Applied
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
			for _, p := range r.Plans {
				if p.Hash == q.Plan {
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
		}
		return nil
	})
	// Runtime startup/readiness is deliberately outside the SQL transaction.
	// Replaying the same plan commits no additional app update, then observes
	// that plan's goal. A later pending plan cannot be substituted for it.
	if err == nil && q.Action == "continue" && result.Applied && result.State != "superseded" && result.Review == nil {
		v, e := m.verify(ctx, id, token, admin)
		if e == nil {
			v = ContinuationResult(result, v)
		}
		return v, e
	}
	return result, err
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
		for _, resource := range []string{"deployments", "services"} {
			if _, e := tx.Get(key(resource, r.Spec.Namespace, r.Spec.Name)); !errors.Is(e, store.ErrNotFound) {
				return fail("STALE", "unselected object already occupies app or Service name")
			}
		}
	} else {
		d, _, e := current(tx, *r)
		if e != nil {
			return e
		}
		if d.Metadata.Generation != r.Generation {
			return fail("STALE", "app was changed outside this operation; operator must reconnect")
		}
		if _, e = checkRoute(tx, *r, d); e != nil {
			return e
		}
		p.BaseSpec, p.BeforeImage = digest(d.Spec), d.Spec.Template.Spec.Containers[0].Image
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

func current(tx *store.Tx, r Record) (apis.Deployment, store.Record, error) {
	var d apis.Deployment
	rec, e := tx.Get(key("deployments", r.Spec.Namespace, r.Spec.Name))
	if e != nil {
		return d, rec, e
	}
	if e = decode(rec, &d); e != nil {
		return d, rec, e
	}
	if r.UID == "" || d.Metadata.UID != r.UID {
		return d, rec, fail("STALE", "selected app identity was removed or replaced")
	}
	return d, rec, eligible(d)
}

func (m *Manager) apply(tx *store.Tx, r *Record, hash string) error {
	if !ValidHash(hash) {
		return fail("INVALID", "continue needs the exact observed plan hash")
	}
	var p *Plan
	for i := range r.Plans {
		if r.Plans[i].Hash == hash {
			p = &r.Plans[i]
			break
		}
	}
	if p == nil {
		return fail("FORBIDDEN", "plan does not belong to this operation")
	}
	if !slices.Contains(r.Spec.Allow, p.Action) {
		return fail("FORBIDDEN", "plan action is not delegated")
	}
	if p.Applied {
		return nil
	} // the database transaction is the execution receipt
	if r.Pending != hash || !p.Approved || p.Discarded {
		return fail("APPROVAL_REQUIRED", "the operator must approve this exact pending plan")
	}
	if !m.now().Before(p.Expires) {
		return fail("EXPIRED", "plan expired; do not replay it")
	}
	if p.BaseUID != r.UID || p.BaseGeneration != r.Generation || p.RouteHash != digest(r.Route) {
		return fail("STALE", "operation target changed after planning")
	}
	var d apis.Deployment
	res := apis.Lookup("apps", "v1", "deployments")
	if p.Action == "deploy" {
		for _, resource := range []string{"deployments", "services"} {
			if _, e := tx.Get(key(resource, r.Spec.Namespace, r.Spec.Name)); !errors.Is(e, store.ErrNotFound) {
				return fail("STALE", "app or Service appeared after approval")
			}
		}
		d = newDeployment(*r, *p)
		objects.Stamp(res, &d)
		if e := res.Validate(&d); len(e) > 0 {
			return fail("INVALID", fmt.Sprint(e))
		}
		if _, e := tx.Create(objects.Put(res, &d)); e != nil {
			return e
		}
		r.UID, r.Generation = d.Metadata.UID, d.Metadata.Generation
		route, e := createService(tx, *r, d)
		if e != nil {
			return e
		}
		r.Route = route
	} else {
		old, rec, e := current(tx, *r)
		if e != nil {
			return e
		}
		if old.Metadata.Generation != p.BaseGeneration || digest(old.Spec) != p.BaseSpec {
			return fail("STALE", "app specification changed after plan approval")
		}
		if _, e = checkRoute(tx, *r, old); e != nil {
			return e
		}
		b, _ := json.Marshal(old)
		if e = json.Unmarshal(b, &d); e != nil {
			return e
		}
		d.Spec.Template.Spec.Containers[0].Image = p.Image
		if d.Spec.Template.Metadata.Annotations == nil {
			d.Spec.Template.Metadata.Annotations = map[string]string{}
		}
		d.Spec.Template.Metadata.Annotations[Marker] = p.Hash
		d.Metadata.Generation++
		if errs := res.Validate(&d); len(errs) > 0 {
			return fail("INVALID", fmt.Sprint(errs))
		}
		if errs := res.ValidateUpdate(&d, &old); len(errs) > 0 {
			return fail("INVALID", fmt.Sprint(errs))
		}
		if _, _, e = tx.Update(objects.Put(res, &d), rec.Rev); e != nil {
			return e
		}
		r.Generation = d.Metadata.Generation
	}
	p.Applied, p.Generation = true, r.Generation
	r.GoalChecks = p.Checks
	r.Pending = ""
	r.StagedImage = ""
	r.StagedChecks = nil
	return nil
}

func newDeployment(r Record, p Plan) apis.Deployment {
	n := int32(1)
	labels := map[string]string{"app.kubernetes.io/name": r.Spec.Name, "shearwater.io/task": r.ID}
	probe, _ := json.Marshal(map[string]any{"tcpSocket": map[string]any{"port": r.Spec.Port}})
	return apis.Deployment{Metadata: apis.ObjectMeta{Name: r.Spec.Name, Namespace: r.Spec.Namespace}, Spec: apis.DeploymentSpec{Replicas: &n, Selector: &apis.LabelSelector{MatchLabels: labels}, Template: apis.PodTemplateSpec{Metadata: apis.ObjectMeta{Labels: labels, Annotations: map[string]string{Marker: p.Hash}}, Spec: apis.PodSpec{Containers: []apis.Container{{Name: "app", Image: p.Image, Ports: []apis.ContainerPort{{ContainerPort: r.Spec.Port}}, ReadinessProbe: probe}}}}}}
}
