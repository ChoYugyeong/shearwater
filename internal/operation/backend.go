package operation

import (
	"net/netip"

	"shearwater/internal/store"
)

// Backend is the part of an operation that knows how an app actually runs.
// The Manager owns everything a model must not be able to influence: the
// delegation token, the exact plan hash, operator approval, the execution
// receipt and the HTTP proof format. A backend only reads and changes the one
// bound app. Every method runs inside the ledger transaction, so a backend
// must keep reads bounded and must not wait for a rollout to finish.
type Backend interface {
	// Bind resolves the selected app when a delegation is created. When the app
	// exists it fills r.UID, r.Generation, r.Route and a missing r.Spec.Image
	// and returns true. When it is absent it returns false without error.
	Bind(tx *store.Tx, r *Record) (bool, error)
	// Reserve checks that a delegation for an absent app could deploy it.
	Reserve(tx *store.Tx, r Record) error
	// Vacant checks, when a deploy plan is made, that nothing else has
	// taken the app or route name since the delegation was created.
	Vacant(tx *store.Tx, r Record) error
	// Base reads the bound app for an update or recovery plan. It fails with
	// STALE when the app changed outside this operation. It returns a digest of
	// the specification the plan is based on and the image currently selected.
	Base(tx *store.Tx, r Record) (spec, image string, err error)
	// Apply executes an approved plan once. It sets r.UID, r.Generation and
	// r.Route to the new goal. Approval and staleness checks already passed.
	Apply(tx *store.Tx, r *Record, p Plan) error
	// Observe reports whether every replica of the goal generation is ready and
	// routed, and which addresses the HTTP checks may use.
	Observe(tx *store.Tx, r Record) (Observation, error)
	// Diagnose appends bounded runtime facts about the bound app.
	Diagnose(tx *store.Tx, r Record, result *Result) error
	// Managed reports whether verify may send HTTP checks to this address.
	Managed(addr netip.Addr) bool
}

// Replica is one running instance of the goal generation that verify probes
// directly, so an older instance behind the same route cannot answer for it.
type Replica struct {
	UID         string
	ContainerID string
	ImageID     string
	IP          string
	Port        int32
}

// Observation is a backend's view of the bound app at one moment.
type Observation struct {
	// External is true when the app changed outside this operation.
	External bool
	// Ready is true when every goal replica is ready and the route reaches
	// only those replicas. Only then are Image, Replicas and Front filled.
	Ready bool
	// Facts are short, model-readable observations in the order found.
	Facts       []string
	Image       string
	Replicas    []Replica
	FrontIP     string
	FrontPort   int32
	Fingerprint string
}

func (m *Manager) backend() Backend {
	if m.Backend != nil {
		return m.Backend
	}
	return Engine{}
}
