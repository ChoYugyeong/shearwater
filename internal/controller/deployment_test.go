package controller

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

var deploymentResource = apis.Lookup("apps", "v1", "deployments")

func TestRolloutBudget(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		replicas             int64
		maxSurge, maxUnavail apis.IntOrString
		surge, unavailable   int64
	}{
		{"round both ways", 7, apis.Str("25%"), apis.Str("25%"), 2, 1},
		{"round small", 1, apis.Str("25%"), apis.Str("25%"), 1, 0},
		{"rounded zero with no surge", 3, apis.Int(0), apis.Str("1%"), 0, 1},
		{"scale to zero", 0, apis.Int(2), apis.Int(2), 0, 0},
		{"large percentages", math.MaxInt32, apis.Str("9223372036854775807%"), apis.Str("200%"), math.MaxInt32, math.MaxInt32},
		{"unavailable capped to desired", 3, apis.Int(1), apis.Int(10), 1, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			strategy := apis.DeploymentStrategy{Type: "RollingUpdate", RollingUpdate: &apis.RollingUpdateDeployment{
				MaxSurge: &tc.maxSurge, MaxUnavailable: &tc.maxUnavail,
			}}
			surge, unavailable := rolloutBudget(strategy, tc.replicas)
			if surge != tc.surge || unavailable != tc.unavailable {
				t.Fatalf("got surge=%d unavailable=%d, want %d/%d", surge, unavailable, tc.surge, tc.unavailable)
			}
		})
	}
}

func testReadyNode(t *testing.T, m Manager) {
	t.Helper()
	create(t, m, apis.Lookup("", "v1", "nodes"), &apis.Node{
		Metadata: apis.ObjectMeta{Name: m.NodeName},
		Status:   apis.NodeStatus{Conditions: []apis.NodeCondition{{Type: "Ready", Status: "True"}}},
	})
}

func readDeployment(t *testing.T, m Manager) *apis.Deployment {
	t.Helper()
	rec, err := m.Store.Get(objects.Key(deploymentResource, "default", "web"))
	if err != nil {
		t.Fatal(err)
	}
	d := &apis.Deployment{}
	if err := json.Unmarshal(rec.Data, d); err != nil {
		t.Fatal(err)
	}
	return d
}

func updateDeployment(t *testing.T, m Manager, f func(*apis.Deployment)) {
	t.Helper()
	d := readDeployment(t, m)
	f(d)
	d.Metadata.Generation++
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		_, _, err := tx.Update(objects.Put(deploymentResource, d), 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func listSets(t *testing.T, m Manager) map[int64]*apis.ReplicaSet {
	t.Helper()
	rows, _, err := m.Store.List("apps", "replicasets", "default")
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[int64]*apis.ReplicaSet)
	for _, rec := range rows {
		rs := &apis.ReplicaSet{}
		if err := json.Unmarshal(rec.Data, rs); err != nil {
			t.Fatal(err)
		}
		num := revisionOf(rs.Meta())
		if result[num] != nil {
			t.Fatalf("duplicate ReplicaSets at revision %d", num)
		}
		result[num] = rs
	}
	return result
}

// Advance just the node side of the controller test. A terminating Pod
// remains in the store until this step, even if another sync has run.
func tickFakeNode(t *testing.T, m Manager, finishDeletes, markReady bool) {
	t.Helper()
	current := pods(t, m)
	err := m.Store.Write(false, func(tx *store.Tx) error {
		for _, p := range current {
			if p.Metadata.DeletionTimestamp != nil {
				if finishDeletes {
					if _, err := tx.Delete(objects.Key(podResource, p.Metadata.Namespace, p.Metadata.Name), 0, objects.Encoder(&p)); err != nil {
						return err
					}
				}
			} else if markReady && p.Spec.NodeName == m.NodeName && !podReady(&p) {
				p.Status.Phase = "Running"
				p.Status.Conditions = append(p.Status.Conditions, apis.PodCondition{Type: "Ready", Status: "True"})
				if _, _, err := tx.Update(objects.Put(podResource, &p), 0); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertBudget(t *testing.T, m Manager, ceiling, floor int) (live, ready, terminating int) {
	t.Helper()
	for _, p := range pods(t, m) {
		if p.Status.Phase == "Failed" || p.Status.Phase == "Succeeded" {
			continue
		}
		live++
		if p.Metadata.DeletionTimestamp != nil {
			terminating++
		} else if podReady(&p) {
			ready++
		}
	}
	if live > ceiling || ready < floor {
		t.Fatalf("rollout crossed its budget: live=%d/%d ready=%d/%d terminating=%d", live, ceiling, ready, floor, terminating)
	}
	return live, ready, terminating
}

func changeImage(d *apis.Deployment, image, cause string) {
	d.Spec.Template.Spec.Containers[0].Image = image
	annotate(d.Meta(), "kubernetes.io/change-cause", cause)
}

func TestRollingUpdateBudgetsAcrossSyncs(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		maxSurge, maxUnavail apis.IntOrString
		ceiling, floor       int
	}{
		{"one surge zero unavailable", apis.Int(1), apis.Int(0), 5, 4},
		{"zero surge one unavailable", apis.Int(0), apis.Str("25%"), 4, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(t)
			testReadyNode(t, m)
			d := testDeployment()
			d.Spec.Replicas = new(int32(4))
			d.Spec.Strategy = apis.DeploymentStrategy{Type: "RollingUpdate", RollingUpdate: &apis.RollingUpdateDeployment{
				MaxSurge: &tc.maxSurge, MaxUnavailable: &tc.maxUnavail,
			}}
			changeImage(d, "busybox:1.36", "initial")
			create(t, m, deploymentResource, d)
			sync(t, m, 4)
			tickFakeNode(t, m, false, true)
			sync(t, m, 2)
			assertBudget(t, m, tc.ceiling, 4)

			updateDeployment(t, m, func(d *apis.Deployment) { changeImage(d, "busybox:1.37", "updated") })
			sync(t, m, 4)
			live, ready, terminating := assertBudget(t, m, tc.ceiling, tc.floor)
			if live != tc.ceiling || ready != tc.floor || terminating != 4-tc.floor {
				t.Fatalf("first wave live=%d ready=%d terminating=%d", live, ready, terminating)
			}
			// Keep the first new Pod unready, then hold the first old Pod
			// terminating. The next create needs a genuinely free slot.
			sync(t, m, 2)
			live, ready, terminating = assertBudget(t, m, tc.ceiling, tc.floor)
			if live != tc.ceiling || ready != tc.floor || terminating != 4-tc.floor {
				t.Fatal("rollout advanced without available capacity")
			}
			tickFakeNode(t, m, false, true)
			sync(t, m, 3)
			live, _, terminating = assertBudget(t, m, tc.ceiling, tc.floor)
			if live != tc.ceiling || terminating != 1 {
				t.Fatalf("expected a terminating Pod to hold a surge slot, live=%d terminating=%d", live, terminating)
			}
			for range 24 {
				tickFakeNode(t, m, true, true)
				sync(t, m, 1)
				assertBudget(t, m, tc.ceiling, tc.floor)
			}
			d = readDeployment(t, m)
			if revisionOf(d.Meta()) != 2 || d.Status.UpdatedReplicas != 4 || d.Status.AvailableReplicas != 4 ||
				d.Status.Replicas != 4 || d.Status.ObservedGeneration != d.Metadata.Generation {
				t.Fatalf("rollout did not settle: %+v", d)
			}
			sets := listSets(t, m)
			if len(sets) != 2 || replicas(sets[1].Spec.Replicas) != 0 || replicas(sets[2].Spec.Replicas) != 4 ||
				sets[1].Metadata.Annotations["kubernetes.io/change-cause"] != "initial" ||
				sets[2].Metadata.Annotations["kubernetes.io/change-cause"] != "updated" {
				t.Fatalf("unexpected rollout history: %+v", sets)
			}
		})
	}
}

func finishRollout(t *testing.T, m Manager) {
	t.Helper()
	for range 12 {
		sync(t, m, 1)
		tickFakeNode(t, m, true, true)
	}
}

func TestDeploymentReusesRevisionAndPrunesOldSets(t *testing.T) {
	m := testManager(t)
	testReadyNode(t, m)
	d := testDeployment()
	d.Metadata.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "client-only"}
	create(t, m, deploymentResource, d)
	finishRollout(t, m)
	first := listSets(t, m)[1]
	if first == nil {
		t.Fatal("missing first revision")
	}
	if _, copied := first.Metadata.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; copied {
		t.Fatal("client-side apply annotation was copied to the ReplicaSet")
	}
	updateDeployment(t, m, func(d *apis.Deployment) { changeImage(d, "busybox:1.37", "changed") })
	finishRollout(t, m)
	updateDeployment(t, m, func(d *apis.Deployment) { changeImage(d, "busybox:1.36", "reverted") })
	finishRollout(t, m)
	sets := listSets(t, m)
	if len(sets) != 2 || sets[3] == nil || sets[3].Metadata.UID != first.Metadata.UID ||
		sets[3].Metadata.Annotations[revisionHistory] != "1" || revisionOf(readDeployment(t, m).Meta()) != 3 {
		t.Fatalf("reused template did not become revision 3: %+v", sets)
	}
	updateDeployment(t, m, func(d *apis.Deployment) { d.Spec.RevisionHistoryLimit = new(int32(0)) })
	finishRollout(t, m)
	if sets = listSets(t, m); len(sets) != 1 || sets[3] == nil {
		t.Fatalf("history limit zero did not preserve exactly the current set: %+v", sets)
	}
	updateDeployment(t, m, func(d *apis.Deployment) { changeImage(d, "busybox:1.38", "after pruning") })
	finishRollout(t, m)
	if sets = listSets(t, m); len(sets) != 1 || sets[4] == nil {
		t.Fatalf("revision did not increase after pruning: %+v", sets)
	}
}

func TestPausedDeploymentScalesWithoutChangingTemplate(t *testing.T) {
	m := testManager(t)
	testReadyNode(t, m)
	create(t, m, deploymentResource, testDeployment())
	finishRollout(t, m)
	updateDeployment(t, m, func(d *apis.Deployment) {
		d.Spec.Paused = true
		d.Spec.Replicas = new(int32(2))
		changeImage(d, "busybox:1.37", "wait for resume")
	})
	finishRollout(t, m)
	sets := listSets(t, m)
	if len(sets) != 1 || sets[1] == nil || replicas(sets[1].Spec.Replicas) != 2 ||
		sets[1].Spec.Template.Spec.Containers[0].Image != "busybox:1.36" {
		t.Fatalf("pause did not preserve the original revision when scaling: %+v", sets)
	}
	updateDeployment(t, m, func(d *apis.Deployment) { d.Spec.Paused = false })
	finishRollout(t, m)
	if sets = listSets(t, m); len(sets) != 2 || sets[2] == nil || replicas(sets[2].Spec.Replicas) != 2 {
		t.Fatalf("resume did not roll out the pending revision: %+v", sets)
	}
}

func TestRecreateWaitsForOldPodsAndPrunesAfterCompletion(t *testing.T) {
	m := testManager(t)
	testReadyNode(t, m)
	create(t, m, deploymentResource, testDeployment())
	finishRollout(t, m)
	updateDeployment(t, m, func(d *apis.Deployment) {
		changeImage(d, "busybox:1.37", "recreate")
		d.Spec.Strategy = apis.DeploymentStrategy{Type: "Recreate"}
		d.Spec.RevisionHistoryLimit = new(int32(0))
	})
	sync(t, m, 4)
	p := pods(t, m)
	sets := listSets(t, m)
	if len(p) != 1 || p[0].Metadata.DeletionTimestamp == nil || len(sets) != 2 ||
		sets[2] == nil || replicas(sets[2].Spec.Replicas) != 0 {
		t.Fatalf("recreate must wait for the old Pod: %+v Pods, %+v ReplicaSets", p, sets)
	}
	finishRollout(t, m)
	sets = listSets(t, m)
	d := readDeployment(t, m)
	if len(sets) != 1 || sets[2] == nil || d.Status.AvailableReplicas != 1 {
		t.Fatalf("recreate did not complete: %+v ReplicaSets, %+v Deployment status", sets, d.Status)
	}
}

func TestRollingUpdateReplacesUnreadyIntermediateRevision(t *testing.T) {
	m := testManager(t)
	testReadyNode(t, m)
	d := testDeployment()
	d.Spec.Replicas = new(int32(3))
	create(t, m, deploymentResource, d)
	finishRollout(t, m)

	updateDeployment(t, m, func(d *apis.Deployment) { changeImage(d, "busybox:1.37", "unready") })
	sync(t, m, 4)
	if live, _, _ := assertBudget(t, m, 4, 3); live != 4 {
		t.Fatalf("expected the unready revision to occupy the only surge slot, got %d Pods", live)
	}
	updateDeployment(t, m, func(d *apis.Deployment) { changeImage(d, "busybox:1.38", "superseded") })
	sync(t, m, 4)
	if live, _, terminating := assertBudget(t, m, 4, 3); live != 4 || terminating != 1 {
		t.Fatalf("expected only the unready Pod to terminate, live=%d terminating=%d", live, terminating)
	}
	for _, p := range pods(t, m) {
		if p.Metadata.DeletionTimestamp != nil && p.Spec.Containers[0].Image != "busybox:1.37" {
			t.Fatal("the rollout removed a Pod from the serving revision")
		}
	}
	for range 24 {
		tickFakeNode(t, m, true, true)
		sync(t, m, 1)
		assertBudget(t, m, 4, 3)
	}
	d = readDeployment(t, m)
	if revisionOf(d.Meta()) != 3 || d.Status.UpdatedReplicas != 3 || d.Status.AvailableReplicas != 3 {
		t.Fatalf("newest revision did not settle: %+v", d)
	}
}

func TestRollingUpdatePreservesRecoveringOldPod(t *testing.T) {
	m := testManager(t)
	testReadyNode(t, m)
	create(t, m, deploymentResource, testDeployment())
	finishRollout(t, m)
	old := pods(t, m)[0]
	updateDeployment(t, m, func(d *apis.Deployment) { changeImage(d, "busybox:1.37", "unready") })
	sync(t, m, 4)
	for j := range old.Status.Conditions {
		if old.Status.Conditions[j].Type == "Ready" {
			old.Status.Conditions[j].Status = "False"
		}
	}
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		_, _, err := tx.Update(objects.Put(podResource, &old), 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sync(t, m, 4)
	for _, p := range pods(t, m) {
		if p.Metadata.UID == old.Metadata.UID && p.Metadata.DeletionTimestamp == nil {
			return
		}
	}
	t.Fatal("rollout removed the only Pod that could restore service while the new Pod was unready")
}

func TestPausedReplacementWaitsForSurgeSlot(t *testing.T) {
	m := testManager(t)
	testReadyNode(t, m)
	d := testDeployment()
	d.Spec.Strategy = apis.DeploymentStrategy{Type: "RollingUpdate", RollingUpdate: &apis.RollingUpdateDeployment{
		MaxSurge: new(apis.Int(0)), MaxUnavailable: new(apis.Int(1)),
	}}
	create(t, m, deploymentResource, d)
	finishRollout(t, m)
	updateDeployment(t, m, func(d *apis.Deployment) { d.Spec.Paused = true })
	sync(t, m, 1)
	p := pods(t, m)[0]
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		rec, err := tx.Get(objects.Key(podResource, p.Metadata.Namespace, p.Metadata.Name))
		if err != nil {
			return err
		}
		_, _, err = objects.Delete(tx, podResource, rec, &p, m.NodeName, nil, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sync(t, m, 5)
	if live, _, term := assertBudget(t, m, 1, 0); live != 1 || term != 1 {
		t.Fatalf("expected just the terminating Pod, got live=%d terminating=%d", live, term)
	}
	finishRollout(t, m)
	got := pods(t, m)
	if len(got) != 1 || got[0].Metadata.UID == p.Metadata.UID || !podReady(&got[0]) {
		t.Fatalf("expected a replacement after the old Pod was removed: %+v", got)
	}
}

func deleteReplicaSet(t *testing.T, m Manager, rs *apis.ReplicaSet) {
	t.Helper()
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		_, err := tx.Delete(objects.Key(rsResource, rs.Metadata.Namespace, rs.Metadata.Name), 0, objects.Encoder(rs))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRolloutAccountsForPodsOfDeletedReplicaSet(t *testing.T) {
	for _, strategy := range []string{"RollingUpdate", "Recreate"} {
		t.Run(strategy, func(t *testing.T) {
			m := testManager(t)
			testReadyNode(t, m)
			d := testDeployment()
			d.Spec.Replicas = new(int32(2))
			d.Spec.Strategy.Type = strategy
			if strategy == "RollingUpdate" {
				d.Spec.Strategy.RollingUpdate = &apis.RollingUpdateDeployment{
					MaxSurge: new(apis.Int(0)), MaxUnavailable: new(apis.Int(1)),
				}
			}
			create(t, m, deploymentResource, d)
			finishRollout(t, m)
			deleteReplicaSet(t, m, listSets(t, m)[1])
			if strategy == "Recreate" {
				updateDeployment(t, m, func(d *apis.Deployment) { d.Spec.Replicas = new(int32(3)) })
			}
			sync(t, m, 8)
			if live, _, term := assertBudget(t, m, 2, 0); live != 2 || term != 2 {
				t.Fatalf("expected the old Pods to hold capacity, got live=%d terminating=%d", live, term)
			}
			finishRollout(t, m)
			d = readDeployment(t, m)
			if d.Status.AvailableReplicas != replicas(d.Spec.Replicas) || revisionOf(d.Meta()) != 2 {
				t.Fatalf("replacement did not converge with the next revision: %+v", d)
			}
		})
	}
}

func TestHistoryWaitsForPodsOfDeletedReplicaSet(t *testing.T) {
	m := testManager(t)
	testReadyNode(t, m)
	create(t, m, deploymentResource, testDeployment())
	finishRollout(t, m)
	updateDeployment(t, m, func(d *apis.Deployment) { changeImage(d, "busybox:1.37", "second") })
	finishRollout(t, m)
	updateDeployment(t, m, func(d *apis.Deployment) {
		changeImage(d, "busybox:1.38", "third")
		d.Spec.RevisionHistoryLimit = new(int32(0))
	})
	sync(t, m, 4)
	tickFakeNode(t, m, false, true)
	sync(t, m, 3)
	deleteReplicaSet(t, m, listSets(t, m)[2])
	sync(t, m, 4)
	if sets := listSets(t, m); sets[1] == nil || sets[3] == nil {
		t.Fatalf("pruned history while a Pod from a deleted revision was still terminating: %+v", sets)
	}
	finishRollout(t, m)
	if sets := listSets(t, m); len(sets) != 1 || sets[3] == nil {
		t.Fatalf("history was not pruned after termination: %+v", sets)
	}
}

func TestWritableDeploymentAnnotationCannotDriveRevisions(t *testing.T) {
	m := testManager(t)
	testReadyNode(t, m)
	d := testDeployment()
	annotate(d.Meta(), revision, "50") // Simulate an annotation stored before the M4 API guard.
	create(t, m, deploymentResource, d)
	finishRollout(t, m)
	if num := revisionOf(readDeployment(t, m).Meta()); num != 1 {
		t.Fatalf("initial revision followed a client annotation: %d", num)
	}
	updateDeployment(t, m, func(d *apis.Deployment) {
		annotate(d.Meta(), revision, "9223372036854775807")
		d.Spec.Replicas = new(int32(2))
	})
	finishRollout(t, m)
	d = readDeployment(t, m)
	if revisionOf(d.Meta()) != 1 || d.Status.AvailableReplicas != 2 || len(listSets(t, m)) != 1 {
		t.Fatalf("client annotation affected the current revision or scaling: %+v", d)
	}
}
