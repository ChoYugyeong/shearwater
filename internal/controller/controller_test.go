package controller

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func testManager(t *testing.T) Manager {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return Manager{Store: db, NodeName: "test"}
}

func create(t *testing.T, m Manager, res *apis.Resource, obj apis.Object) {
	t.Helper()
	objects.Stamp(res, obj)
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		_, err := tx.Create(objects.Put(res, obj))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func sync(t *testing.T, m Manager, n int) {
	t.Helper()
	for range n {
		if err := m.Sync(); err != nil {
			t.Fatal(err)
		}
	}
}

func pods(t *testing.T, m Manager) []apis.Pod {
	t.Helper()
	rows, _, err := m.Store.List("", "pods", "default")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]apis.Pod, len(rows))
	for i, r := range rows {
		if err := json.Unmarshal(r.Data, &out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func testDeployment() *apis.Deployment {
	labels := map[string]string{"app": "web"}
	return &apis.Deployment{Metadata: apis.ObjectMeta{Name: "web", Namespace: "default"}, Spec: apis.DeploymentSpec{
		Selector: &apis.LabelSelector{MatchLabels: labels},
		Template: apis.PodTemplateSpec{Metadata: apis.ObjectMeta{Labels: labels},
			Spec: apis.PodSpec{Containers: []apis.Container{{Name: "web", Image: "busybox:1.36"}}}},
	}}
}

func TestDeploymentConvergesAndBindsWhenNodeReady(t *testing.T) {
	m := testManager(t)
	res := apis.Lookup("apps", "v1", "deployments")
	create(t, m, res, testDeployment())
	sync(t, m, 6)
	p := pods(t, m)
	if len(p) != 1 || p[0].Spec.NodeName != "" || len(p[0].Status.Conditions) != 1 ||
		p[0].Status.Conditions[0].Reason != "Unschedulable" {
		t.Fatalf("expected one unscheduled Pod: %+v", p)
	}
	create(t, m, apis.Lookup("", "v1", "nodes"), &apis.Node{Metadata: apis.ObjectMeta{Name: "test"},
		Status: apis.NodeStatus{Conditions: []apis.NodeCondition{{Type: "Ready", Status: "True"}}}})
	sync(t, m, 5)
	p = pods(t, m)
	if len(p) != 1 || p[0].Spec.NodeName != "test" || p[0].Status.Conditions[0].Status != "True" {
		t.Fatalf("expected one bound Pod: %+v", p)
	}
	before, _ := m.Store.Rev()
	sync(t, m, 2)
	if after, _ := m.Store.Rev(); after != before {
		t.Fatalf("settled controller changed revision: %d -> %d", before, after)
	}
}

func TestReplicaSetAdoptsAndReplaces(t *testing.T) {
	m := testManager(t)
	d := testDeployment()
	p := &apis.Pod{Metadata: apis.ObjectMeta{Name: "loose", Namespace: "default", Labels: d.Spec.Template.Metadata.Labels},
		Spec: d.Spec.Template.Spec}
	create(t, m, podResource, p)
	rs := &apis.ReplicaSet{Metadata: apis.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: apis.ReplicaSetSpec{Selector: d.Spec.Selector, Template: d.Spec.Template}}
	create(t, m, rsResource, rs)
	sync(t, m, 3)
	got := pods(t, m)
	if len(got) != 1 || got[0].Metadata.UID != p.Metadata.UID || len(got[0].Metadata.OwnerReferences) != 1 ||
		got[0].Metadata.OwnerReferences[0].UID != rs.Metadata.UID {
		t.Fatalf("expected adopted Pod: %+v", got)
	}
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		_, err := tx.Delete(objects.Key(podResource, "default", "loose"), 0, objects.Encoder(p))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sync(t, m, 3)
	got = pods(t, m)
	if len(got) != 1 || got[0].Metadata.UID == p.Metadata.UID {
		t.Fatalf("expected a replacement Pod: %+v", got)
	}
}

func TestForegroundWaitsForBlockingGrandchild(t *testing.T) {
	m := testManager(t)
	res := apis.Lookup("apps", "v1", "deployments")
	d := testDeployment()
	create(t, m, res, d)
	sync(t, m, 5)
	p := pods(t, m)[0]
	dKey := objects.Key(res, "default", d.Metadata.Name)
	pKey := objects.Key(podResource, "default", p.Metadata.Name)
	p.Metadata.Finalizers = []string{"example.com/test"}
	d.Metadata.Finalizers = []string{objects.ForegroundFinalizer}
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		if _, _, err := tx.Update(objects.Put(podResource, &p), 0); err != nil {
			return err
		}
		r, err := tx.Get(dKey)
		if err != nil {
			return err
		}
		_, _, err = objects.Delete(tx, res, r, d, m.NodeName, nil, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sync(t, m, 5)
	if _, err := m.Store.Get(dKey); err != nil {
		t.Fatalf("Deployment should wait for its blocking grandchild: %v", err)
	}
	if got := pods(t, m); len(got) != 1 || got[0].Metadata.DeletionTimestamp == nil {
		t.Fatalf("expected a terminating Pod: %+v", got)
	}
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		_, err := tx.Delete(pKey, 0, objects.Encoder(&p))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sync(t, m, 4)
	if _, err := m.Store.Get(dKey); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Deployment should be gone after its dependents, got %v", err)
	}
}

func TestDeploymentSelectorConstrainsHashLabel(t *testing.T) {
	m := testManager(t)
	d := testDeployment()
	d.Spec.Selector.MatchLabels[templateHash] = "explicit"
	create(t, m, apis.Lookup("apps", "v1", "deployments"), d)
	sync(t, m, 10)
	sets, _, err := m.Store.List("apps", "replicasets", "default")
	got := pods(t, m)
	if err != nil || len(sets) != 1 || len(got) != 1 || got[0].Metadata.Labels[templateHash] != "explicit" {
		t.Fatalf("expected one ReplicaSet and one Pod matching the explicit selector: %d sets, %v pods, %v", len(sets), got, err)
	}
}

func TestForegroundBreaksBlockingOwnershipCycle(t *testing.T) {
	m := testManager(t)
	makePod := func(name string) *apis.Pod {
		return &apis.Pod{Metadata: apis.ObjectMeta{Name: name, Namespace: "default"},
			Spec: testDeployment().Spec.Template.Spec}
	}
	a, b := makePod("a"), makePod("b")
	create(t, m, podResource, a)
	create(t, m, podResource, b)
	ref := func(p *apis.Pod) apis.OwnerReference {
		yes := true
		return apis.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: p.Metadata.Name,
			UID: p.Metadata.UID, BlockOwnerDeletion: &yes}
	}
	a.Metadata.OwnerReferences = []apis.OwnerReference{ref(b)}
	b.Metadata.OwnerReferences = []apis.OwnerReference{ref(a)}
	a.Metadata.Finalizers = []string{objects.ForegroundFinalizer}
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		if _, _, err := tx.Update(objects.Put(podResource, b), 0); err != nil {
			return err
		}
		r, err := tx.Get(objects.Key(podResource, "default", "a"))
		if err != nil {
			return err
		}
		_, _, err = objects.Delete(tx, podResource, r, a, m.NodeName, nil, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sync(t, m, 6)
	if got := pods(t, m); len(got) != 0 {
		t.Fatalf("blocking cycle left Pods behind: %+v", got)
	}
}
