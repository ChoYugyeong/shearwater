// Package operation implements bounded, server-owned app operations. Models
// choose an action; they do not supply arbitrary API requests, approve their own
// plans, or decide that an HTTP response from an older rollout proves success.
package operation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

const Prefix = "/shearwater/operations"
const Marker = "shearwater.io/operation-plan"

type Check struct {
	Path     string `json:"path"`
	Contains string `json:"contains,omitempty"`
}

// Spec is reviewed by the operator at delegation time. A bearer token receives
// only these actions until Expires, and only this app identity after it is bound.
// A new app may be created from our fixed one-container template; the agent
// cannot provide host mounts, commands, environment variables, or raw manifests.
type Spec struct {
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Image     string    `json:"image,omitempty"`
	Service   string    `json:"service,omitempty"`
	Port      int32     `json:"port,omitempty"`
	Checks    []Check   `json:"checks"`
	Allow     []string  `json:"allow"`
	Expires   time.Time `json:"expires"`
}

type Route struct {
	Name     string            `json:"name"`
	UID      string            `json:"uid"`
	IP       string            `json:"ip"`
	Selector map[string]string `json:"selector"`
	Port     apis.ServicePort  `json:"port"`
}

// No full Deployment or environment values appear in a plan returned to AI.
// BaseSpec binds those fields by hash; execution reloads them and changes only
// the image plus a rollout marker. A status heartbeat does not invalidate it.
type Plan struct {
	Hash           string    `json:"hash"`
	Action         string    `json:"action"`
	Image          string    `json:"image"`
	BeforeImage    string    `json:"beforeImage,omitempty"`
	BaseUID        string    `json:"baseUID,omitempty"`
	BaseGeneration int64     `json:"baseGeneration"`
	BaseSpec       string    `json:"baseSpec"`
	RouteHash      string    `json:"routeHash"`
	Checks         []Check   `json:"checks"`
	Created        time.Time `json:"created"`
	Expires        time.Time `json:"expires"`
	Approved       bool      `json:"approved"`
	Applied        bool      `json:"applied"`
	Discarded      bool      `json:"discarded,omitempty"`
	Generation     int64     `json:"generation,omitempty"`
}

type Proof struct {
	At          time.Time   `json:"at"`
	Generation  int64       `json:"generation"`
	Image       string      `json:"image"`
	ImagePinned bool        `json:"imageContentPinned"`
	Checks      []Check     `json:"checks"`
	Pods        []PodProof  `json:"pods"`
	Service     []HTTPProof `json:"service"`
}

type PodProof struct {
	UID         string      `json:"uid"`
	ContainerID string      `json:"containerID"`
	ImageID     string      `json:"imageID"`
	Checks      []HTTPProof `json:"checks"`
}
type HTTPProof struct {
	Path   string `json:"path"`
	Status int    `json:"status"`
	SHA256 string `json:"bodySHA256,omitempty"`
	Bytes  int    `json:"bytes"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
}

type Record struct {
	LastCheck    *Proof  `json:"lastCheck,omitempty"`
	ID           string  `json:"id"`
	TokenHash    string  `json:"tokenHash"` // internal only; the token itself is never stored
	Revoked      bool    `json:"revoked"`
	Spec         Spec    `json:"spec"`
	UID          string  `json:"uid,omitempty"`
	Generation   int64   `json:"generation"`
	Route        *Route  `json:"route,omitempty"`
	Pending      string  `json:"pending,omitempty"`
	Plans        []Plan  `json:"plans"`
	StagedImage  string  `json:"stagedImage,omitempty"`
	StagedChecks []Check `json:"stagedChecks,omitempty"`
	GoalChecks   []Check `json:"goalChecks"`
	LastGood     *Proof  `json:"lastGood,omitempty"`
}

type Request struct {
	Action string  `json:"action"`
	Image  string  `json:"image,omitempty"`
	Plan   string  `json:"plan,omitempty"`
	Checks []Check `json:"checks,omitempty"` // operator stage only
}

type Result struct {
	ProofID       string     `json:"proofID,omitempty"`
	VerifiedPods  int        `json:"verifiedPods,omitempty"`
	CheckedAt     *time.Time `json:"checkedAt,omitempty"`
	LastPlan      string     `json:"lastPlan,omitempty"`
	StagedImage   string     `json:"stagedImage,omitempty"`
	VerifiedImage string     `json:"verifiedImage,omitempty"`
	Format        string     `json:"format"`
	Operation     string     `json:"operation"`
	App           string     `json:"app"`
	UID           string     `json:"uid,omitempty"`
	Generation    int64      `json:"goalGeneration"`
	State         string     `json:"state"`
	Allowed       []string   `json:"allowedActions"`
	Expires       time.Time  `json:"expires"`
	Plan          string     `json:"plan,omitempty"`
	Approved      bool       `json:"approved"`
	Applied       bool       `json:"applied"`
	Review        *Plan      `json:"review,omitempty"`
	Facts         []string   `json:"facts"`
	Unknown       []string   `json:"unknown"`
	Next          string     `json:"next"`
	HTTP          any        `json:"http"`
	Evidence      *Proof     `json:"evidence,omitempty"`
	Handoff       string     `json:"handoff,omitempty"`
}

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string        { return e.Code + ": " + e.Message }
func fail(code, message string) error { return &Error{code, message} }

type Manager struct {
	// Store holds the operation ledger. The engine backend also keeps the app
	// objects there, so application and receipt commit together.
	Store *store.Store
	// Backend runs the bound app. Nil selects Shearwater's own engine.
	Backend Backend
	Now     func() time.Time
	// Probe is injectable for deterministic tests. Production always uses the
	// restricted direct-IP HTTP implementation, never an agent-supplied URL.
	Probe func(context.Context, string, int32, Check) HTTPProof
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func randomID(n int) string { b := make([]byte, n); rand.Read(b); return hex.EncodeToString(b) }
func ValidID(id string) bool {
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 16 && id == strings.ToLower(id)
}
func ValidHash(id string) bool {
	b, e := hex.DecodeString(id)
	return e == nil && len(b) == 32 && id == strings.ToLower(id)
}
func validImage(s string) bool {
	return s != "" && len(s) <= 1024 && strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
func pinned(s string) bool { _, d, ok := strings.Cut(s, "@sha256:"); return ok && ValidHash(d) }
func validateChecks(checks []Check) error {
	if len(checks) < 1 || len(checks) > 4 {
		return fail("INVALID", "provide 1..4 HTTP checks")
	}
	for _, c := range checks {
		u, e := url.Parse(c.Path)
		if e != nil || !strings.HasPrefix(c.Path, "/") || strings.HasPrefix(c.Path, "//") || u.IsAbs() || u.Host != "" || u.Fragment != "" || len(c.Path) > 1024 || len(c.Contains) > 1024 || strings.ContainsAny(c.Path, "\r\n\x00") {
			return fail("INVALID", "checks require bounded relative HTTP paths")
		}
	}
	return nil
}
func load(tx *store.Tx, id string) (Record, error) {
	var r Record
	b, e := tx.Operation(id)
	if e != nil {
		return r, e
	}
	e = json.Unmarshal(b, &r)
	return r, e
}
func save(tx *store.Tx, r Record, create bool) error {
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if len(b) > 256<<10 {
		return fail("LIMIT", "operation history exceeds limit; create a new delegation")
	}
	return tx.PutOperation(r.ID, b, create)
}
func key(resource, ns, name string) store.Key {
	g := ""
	if resource == "deployments" || resource == "replicasets" {
		g = "apps"
	}
	return store.Key{Group: g, Resource: resource, Namespace: ns, Name: name}
}
func decode(rec store.Record, v any) error {
	if e := json.Unmarshal(rec.Data, v); e != nil {
		return fmt.Errorf("invalid stored %s: %w", rec.Key, e)
	}
	return nil
}
