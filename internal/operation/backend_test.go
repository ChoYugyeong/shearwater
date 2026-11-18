package operation

import (
	"net/netip"
	"slices"
	"testing"

	"shearwater/internal/store"
)

// tracing runs the real engine backend and records which backend steps the
// Manager used. It refuses every probe address so no test touches a network.
type tracing struct {
	Engine
	calls []string
}

func (b *tracing) Vacant(tx *store.Tx, r Record) error {
	b.calls = append(b.calls, "vacant")
	return b.Engine.Vacant(tx, r)
}
func (b *tracing) Base(tx *store.Tx, r Record) (string, string, error) {
	b.calls = append(b.calls, "base")
	return b.Engine.Base(tx, r)
}
func (b *tracing) Apply(tx *store.Tx, r *Record, p Plan) error {
	b.calls = append(b.calls, "apply:"+p.Action)
	return b.Engine.Apply(tx, r, p)
}
func (b *tracing) Observe(tx *store.Tx, r Record) (Observation, error) {
	b.calls = append(b.calls, "observe")
	return b.Engine.Observe(tx, r)
}
func (b *tracing) Diagnose(tx *store.Tx, r Record, result *Result) error {
	b.calls = append(b.calls, "diagnose")
	return b.Engine.Diagnose(tx, r, result)
}
func (b *tracing) Managed(netip.Addr) bool { return false }

func TestManagerRunsAppStepsAndProbesThroughItsBackend(t *testing.T) {
	f := newFixture(t)
	b := &tracing{}
	f.m.Backend, f.m.Probe = b, nil
	f.deploy()
	f.ready("a")
	r := f.call(false, Request{Action: "verify"})
	if r.State != "http_failed" || r.VerifiedPods != 1 {
		t.Fatal("verify did not reach the probe stage through the backend", r)
	}
	proof := f.stored().LastCheck
	if proof == nil || len(proof.Pods) != 1 || len(proof.Service) != 1 {
		t.Fatal("missing per-replica and front proof", proof)
	}
	for _, hp := range append(slices.Clone(proof.Pods[0].Checks), proof.Service...) {
		if hp.Passed || hp.Reason != "invalid_managed_address" {
			t.Fatal("the default probe ignored the backend's address policy", hp)
		}
	}
	f.call(false, Request{Action: "diagnose"})
	// continue observes once right after applying; verify observes before and
	// after its HTTP checks so a rollout during I/O cannot be saved as proof.
	want := []string{"vacant", "apply:deploy", "observe", "observe", "observe", "diagnose"}
	if !slices.Equal(b.calls, want) {
		t.Fatalf("backend calls = %v, want %v", b.calls, want)
	}
	f.call(false, Request{Action: "update", Image: imageTwo})
	if b.calls[len(b.calls)-1] != "base" {
		t.Fatal("update plan did not read its base from the backend", b.calls)
	}
}

func TestEngineProbesOnlyManagedIPv4Networks(t *testing.T) {
	for addr, want := range map[string]bool{
		"10.244.0.10": true,  // Pod network
		"10.96.0.1":   true,  // Service network
		"127.0.0.1":   false, // host loopback
		"172.17.0.2":  false, // another runtime's bridge
		"169.254.0.1": false, // link-local metadata range
		"::1":         false,
	} {
		if got := (Engine{}).Managed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Managed(%s) = %t, want %t", addr, got, want)
		}
	}
}
