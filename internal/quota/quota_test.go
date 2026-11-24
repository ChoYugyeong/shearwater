package quota

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"shearwater/internal/admission"
	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func TestAccounting(t *testing.T) {
	key := store.Key{Resource: "pods", Namespace: "team-a"}
	rows := []struct {
		deleting bool
		json     string
	}{
		{false, `{"spec":{"containers":[{"resources":{"requests":{"cpu":"200m"},"limits":{"cpu":"500m"}}},{"resources":{"limits":{"cpu":"300m"}}}]}}`},
		{false, `{"status":{"phase":"Failed"}}`},
		{false, `{"status":{"phase":"Succeeded"}}`},
		{true, `{}`},
	}
	var items []item
	for _, row := range rows {
		i, err := load(key, row.deleting, []byte(row.json))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, i)
	}
	for k, want := range map[string]apis.Quantity{"requests.cpu": "500m", "limits.cpu": "800m", "pods": "1", "count/pods": "1", "services": "0"} {
		n, err := usage(items, k)
		if err != nil || quantity(n) != want {
			t.Errorf("usage(%q) = %v, %v; want %s", k, n, err, want)
		}
	}
	deployment := item{key: store.Key{Group: "apps", Resource: "deployments"}, active: true}
	if n, _ := deployment.amount("count/deployments.apps", false); n.Sign() == 0 {
		t.Error("deployment not counted")
	}
	claim, err := load(store.Key{Resource: "persistentvolumeclaims"}, false, []byte(`{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := claim.amount("requests.storage", false); quantity(n) != "2147483648" {
		t.Fatalf("claim storage = %s", n)
	}
}

func TestConcurrentAdmissionAndStatus(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := New(db)
	res := apis.Lookup("", "v1", "resourcequotas")
	q := &apis.ResourceQuota{
		Metadata: apis.ObjectMeta{Name: "budget", Namespace: "team-a"},
		Spec:     apis.ResourceQuotaSpec{Hard: map[string]apis.Quantity{"pods": "1"}},
	}
	objects.Stamp(res, q)
	if err := db.Write(false, func(tx *store.Tx) error {
		_, err := tx.Create(objects.Put(res, q))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	create := func(name string, dryRun bool) error {
		pods := apis.Lookup("", "v1", "pods")
		p := &apis.Pod{Metadata: apis.ObjectMeta{Name: name, Namespace: "team-a"}}
		objects.Stamp(pods, p)
		return db.Write(dryRun, func(tx *store.Tx) error {
			err := c.Admit(admission.Request{Resource: pods, Operation: admission.Create, Namespace: "team-a", Name: name, Tx: tx, Object: p, DryRun: dryRun})
			if err != nil {
				return err
			}
			_, err = tx.Create(objects.Put(pods, p))
			return err
		})
	}
	if err := create("preview", true); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- create(name, false)
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else {
			var denied *admission.Denied
			if !errors.As(err, &denied) || denied.ErrCode != "QUOTA_EXCEEDED" {
				t.Fatal(err)
			}
		}
	}
	if success != 1 {
		t.Fatalf("accepted %d pods, want 1", success)
	}
	key := objects.Key(res, "team-a", "budget")
	if err := c.refresh(key); err != nil {
		t.Fatal(err)
	}
	row, err := db.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	var got apis.ResourceQuota
	if err := json.Unmarshal(row.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Hard["pods"] != "1" || got.Status.Used["pods"] != "1" {
		t.Fatalf("unexpected quota status %+v", got.Status)
	}
}

func TestDenialAndUpdate(t *testing.T) {
	q := &apis.ResourceQuota{Metadata: apis.ObjectMeta{Name: "budget"}, Spec: apis.ResourceQuotaSpec{Hard: map[string]apis.Quantity{"pods": "1"}}}
	pod := item{key: store.Key{Resource: "pods"}, active: true, pod: &apis.Pod{Spec: apis.PodSpec{Containers: []apis.Container{{Name: "worker"}}}}}
	items := []item{pod}
	var denied *admission.Denied
	err := check(q, items, pod, item{})
	if !errors.As(err, &denied) || denied.Code != 403 || denied.Message != "exceeded quota: budget, requested: pods=1, used: pods=1, limited: pods=1" {
		t.Fatalf("quota refusal = %v", err)
	}
	if err := check(q, items, pod, pod); err != nil {
		t.Fatalf("unchanged usage refused: %v", err)
	}
	q.Spec.Hard = map[string]apis.Quantity{"limits.memory": "64Mi"}
	if err := check(q, nil, pod, item{}); !errors.As(err, &denied) || denied.Message != "failed quota: budget: must specify limits.memory for: worker" {
		t.Fatalf("missing limit refusal = %v", err)
	}
}
