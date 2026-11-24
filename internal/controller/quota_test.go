package controller

import (
	"errors"
	"strings"
	"testing"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

func TestReplicaSetPodsGoThroughAdmissionInQuotaNamespaces(t *testing.T) {
	m := testManager(t)
	addNode(t, m, "test", nil, "8", nil)
	admitted := 0
	m.AdmitPod = func(tx *store.Tx, p *apis.Pod) error {
		// Like the quota plugin: count what is committed in the namespace.
		rows, _, err := m.Store.List("", "pods", p.Metadata.Namespace)
		if err != nil {
			return err
		}
		if len(rows) >= 2 {
			return errors.New("exceeded quota: pods, requested: pods=1, used: pods=2, limited: pods=2")
		}
		admitted++
		return nil
	}
	create(t, m, apis.Lookup("", "v1", "resourcequotas"), &apis.ResourceQuota{Metadata: apis.ObjectMeta{Name: "q", Namespace: "default"}})
	d := testDeployment()
	rs := &apis.ReplicaSet{Metadata: apis.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: apis.ReplicaSetSpec{Replicas: new(int32(5)), Selector: d.Spec.Selector, Template: d.Spec.Template}}
	create(t, m, apis.Lookup("apps", "v1", "replicasets"), rs)
	sync(t, m, 10)
	if got := len(pods(t, m)); got != 2 || admitted != 2 {
		t.Fatalf("pods %d, admitted %d; want 2 and 2", got, admitted)
	}
	events, _, _ := m.Store.List("", "events", "default")
	found := false
	for _, e := range events {
		if strings.Contains(string(e.Data), "FailedCreate") && strings.Contains(string(e.Data), "exceeded quota") {
			found = true
		}
	}
	if !found {
		t.Fatal("no FailedCreate event on the ReplicaSet")
	}
	// A refusal that repeats must not write on every pass.
	before, _ := m.Store.Rev()
	sync(t, m, 5)
	if after, _ := m.Store.Rev(); after != before {
		t.Fatalf("repeated refusals kept writing: revision %d -> %d", before, after)
	}
}
