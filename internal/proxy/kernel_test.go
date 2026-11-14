package proxy

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/drain"
	"shearwater/internal/objects"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

type fakeDataPath struct {
	calls                 []string
	active                int
	failRules, failCounts bool
	forgot                netip.Addr
	onReconcile           func()
}

func (f *fakeDataPath) Reconcile(context.Context, []runtime.ServiceRoute) error {
	f.calls = append(f.calls, "withdraw")
	if f.onReconcile != nil {
		f.onReconcile()
	}
	if f.failRules {
		return errors.New("rules unavailable")
	}
	return nil
}
func (f *fakeDataPath) Counts(context.Context) (map[netip.Addr]int, error) {
	f.calls = append(f.calls, "count")
	if f.failCounts {
		return nil, errors.New("dump interrupted")
	}
	return map[netip.Addr]int{netip.MustParseAddr("10.244.0.5"): f.active}, nil
}
func (f *fakeDataPath) ForgetPod(_ context.Context, ip netip.Addr) error {
	f.calls = append(f.calls, "forget")
	f.forgot = ip
	return nil
}

func TestKernelDrainAcknowledgesOnlyAfterWithdrawalAndCompleteDump(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res := apis.Lookup("", "v1", "pods")
	p := &apis.Pod{Metadata: apis.ObjectMeta{Name: "web", Namespace: "default", Annotations: map[string]string{drain.Annotation: "intent"}}, Status: apis.PodStatus{PodIP: "10.244.0.5"}}
	objects.Stamp(res, p)
	p.Status = apis.PodStatus{PodIP: "10.244.0.5"}
	if err := st.Write(false, func(tx *store.Tx) error {
		if _, err := tx.AllocIP("pod", p.Metadata.UID, p.Status.PodIP, nil); err != nil {
			return err
		}
		_, err := tx.Create(objects.Put(res, p))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeDataPath{active: 1}
	state := &drain.State{}
	m := &KernelManager{Manager: Manager{Store: st}, DataPath: f, Drain: state}
	if err := m.sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"withdraw", "count"}) || state.Ready(p.Metadata.UID, "intent", time.Now()) {
		t.Fatal("bad ordering or early deletion")
	}
	f.active = 0
	if err := m.sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !state.Ready(p.Metadata.UID, "intent", time.Now()) {
		t.Fatal("not drained")
	}
	f.failCounts = true
	if m.sync(t.Context()) == nil || state.Ready(p.Metadata.UID, "intent", time.Now().Add(time.Hour)) {
		t.Fatal("failed dump retained acknowledgement")
	}
	f.failCounts = false
	f.failRules = true
	f.calls = nil
	if m.sync(t.Context()) == nil || !slices.Equal(f.calls, []string{"withdraw"}) {
		t.Fatal("counted before rule success")
	}
	if m.RetirePod(t.Context(), p.Metadata.UID) == nil {
		t.Fatal("retired live Pod")
	}
	if err := st.Write(false, func(tx *store.Tx) error {
		rec, err := tx.Get(objects.Key(res, "default", "web"))
		if err != nil {
			return err
		}
		_, err = tx.Delete(rec.Key, rec.Rev, objects.Encoder(p))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if m.RetirePod(t.Context(), p.Metadata.UID) == nil || f.forgot.IsValid() {
		t.Fatal("forgot connection despite failed withdrawal")
	}
	f.failRules = false
	f.calls = nil
	if err := m.RetirePod(t.Context(), p.Metadata.UID); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"withdraw", "forget"}) || f.forgot.String() != p.Status.PodIP {
		t.Fatal("IP reuse barrier not serialized")
	}
}

func TestDrainArrivingDuringRuleInstallNeedsAnotherPass(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res := apis.Lookup("", "v1", "pods")
	p := &apis.Pod{Metadata: apis.ObjectMeta{Name: "web", Namespace: "default"}}
	objects.Stamp(res, p)
	p.Status.PodIP = "10.244.0.5"
	if err := st.Write(false, func(tx *store.Tx) error {
		if _, err := tx.AllocIP("pod", p.Metadata.UID, p.Status.PodIP, nil); err != nil {
			return err
		}
		_, err := tx.Create(objects.Put(res, p))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeDataPath{}
	state := &drain.State{}
	m := &KernelManager{Manager: Manager{Store: st}, DataPath: f, Drain: state}
	f.onReconcile = func() {
		f.onReconcile = nil
		if err := st.Write(false, func(tx *store.Tx) error {
			rec, err := tx.Get(objects.Key(res, "default", "web"))
			if err != nil {
				return err
			}
			p.Metadata.Annotations = map[string]string{drain.Annotation: "arrived-during-install"}
			_, _, err = tx.Update(objects.Put(res, p), rec.Rev)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state.Ready(p.Metadata.UID, "arrived-during-install", time.Now().Add(time.Hour)) || slices.Contains(f.calls, "count") {
		t.Fatal("acknowledged a drain that was absent from the installed snapshot")
	}
	if err := m.sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !state.Ready(p.Metadata.UID, "arrived-during-install", time.Now()) {
		t.Fatal("next pass did not acknowledge withdrawal")
	}
}
