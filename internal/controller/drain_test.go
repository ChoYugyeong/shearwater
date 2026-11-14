package controller

import (
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/drain"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func TestReplicaSetDrainsBeforeDeletionAndCanCancel(t *testing.T) {
	m := testManager(t)
	m.Drain = &drain.State{}
	d := testDeployment()
	rsRes := apis.Lookup("apps", "v1", "replicasets")
	rs := &apis.ReplicaSet{Metadata: apis.ObjectMeta{Name: "web", Namespace: "default"}, Spec: apis.ReplicaSetSpec{Replicas: new(int32(1)), Selector: d.Spec.Selector, Template: d.Spec.Template}}
	create(t, m, rsRes, rs)
	p := &apis.Pod{Metadata: apis.ObjectMeta{Name: "p", Namespace: "default", Labels: d.Spec.Template.Metadata.Labels}, Spec: d.Spec.Template.Spec,
		Status: apis.PodStatus{Phase: "Running", PodIP: "10.244.0.5", Conditions: []apis.PodCondition{{Type: "Ready", Status: "True"}}}}
	p.Spec.NodeName = "test"
	p.Spec.TerminationGracePeriodSeconds = new(int64(1))
	create(t, m, podResource, p)
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		rec, err := tx.Get(objects.Key(podResource, "default", "p"))
		if err != nil {
			return err
		}
		p.Status = apis.PodStatus{Phase: "Running", PodIP: "10.244.0.5", Conditions: []apis.PodCondition{{Type: "Ready", Status: "True"}}}
		_, _, err = tx.Update(objects.Put(podResource, p), rec.Rev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sync(t, m, 2)
	scale := func(n int32) {
		t.Helper()
		if err := m.Store.Write(false, func(tx *store.Tx) error {
			rec, err := tx.Get(objects.Key(rsRes, "default", "web"))
			if err != nil {
				return err
			}
			rs.Spec.Replicas = &n
			_, _, err = tx.Update(objects.Put(rsRes, rs), rec.Rev)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		sync(t, m, 2)
	}
	scale(0)
	got := pods(t, m)[0]
	token := got.Metadata.Annotations[drain.Annotation]
	if token == "" || got.Metadata.DeletionTimestamp != nil || podReady(&got) {
		t.Fatal("Pod terminated before forwarding acknowledgement")
	}
	m.Drain.Observe(time.Now(), map[string]drain.Observation{got.Metadata.UID: {Token: token, Active: 1}})
	sync(t, m, 2)
	if pods(t, m)[0].Metadata.DeletionTimestamp != nil {
		t.Fatal("active connection interrupted")
	}
	scale(1)
	if pods(t, m)[0].Metadata.Annotations[drain.Annotation] != "" {
		t.Fatal("scale-up did not cancel pending retirement")
	}
	scale(0)
	got = pods(t, m)[0]
	token = got.Metadata.Annotations[drain.Annotation]
	m.Drain.Observe(time.Now(), map[string]drain.Observation{got.Metadata.UID: {Token: token, Active: 0}})
	sync(t, m, 1)
	got = pods(t, m)[0]
	if got.Metadata.DeletionTimestamp == nil || *got.Metadata.DeletionGracePeriodSeconds != 1 {
		t.Fatal("did not begin original one-second grace after drain")
	}
}
