package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

var nodeResource = apis.Lookup("", "v1", "nodes")

func addNode(t *testing.T, m Manager, name string, labels map[string]string, cpu string, mutate func(*apis.Node)) {
	t.Helper()
	n := &apis.Node{Metadata: apis.ObjectMeta{Name: name, Labels: labels},
		Status: apis.NodeStatus{Conditions: []apis.NodeCondition{{Type: "Ready", Status: "True"}},
			Allocatable: map[string]apis.Quantity{"cpu": apis.Quantity(cpu), "memory": "4Gi", "pods": "110"}}}
	if mutate != nil {
		mutate(n)
	}
	create(t, m, nodeResource, n)
}

func newPod(name string, mutate func(*apis.Pod)) *apis.Pod {
	p := &apis.Pod{Metadata: apis.ObjectMeta{Name: name, Namespace: "default"},
		Spec: apis.PodSpec{RestartPolicy: "Always", Containers: []apis.Container{{Name: "c", Image: "busybox"}}}}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func podByName(t *testing.T, m Manager, name string) apis.Pod {
	t.Helper()
	for _, p := range pods(t, m) {
		if p.Metadata.Name == name {
			return p
		}
	}
	t.Fatalf("no pod %s", name)
	return apis.Pod{}
}

func condition(p apis.Pod, typ string) apis.PodCondition {
	for _, c := range p.Status.Conditions {
		if c.Type == typ {
			return c
		}
	}
	return apis.PodCondition{}
}

func TestSchedulerSpreadsAReplicaSetAcrossNodes(t *testing.T) {
	m := testManager(t)
	m.NodeName = "a"
	addNode(t, m, "a", nil, "4", nil)
	addNode(t, m, "b", nil, "4", nil)
	addNode(t, m, "c", nil, "4", func(n *apis.Node) { n.Status.Conditions[0].Status = "False" })
	d := testDeployment()
	rs := &apis.ReplicaSet{Metadata: apis.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: apis.ReplicaSetSpec{Replicas: new(int32(4)), Selector: d.Spec.Selector, Template: d.Spec.Template}}
	create(t, m, apis.Lookup("apps", "v1", "replicasets"), rs)
	sync(t, m, 3)
	per := map[string]int{}
	for _, p := range pods(t, m) {
		per[p.Spec.NodeName]++
	}
	if per["a"] != 2 || per["b"] != 2 || per["c"] != 0 {
		t.Fatalf("placement: %v", per)
	}
}

func TestSchedulerFiltersAndExplains(t *testing.T) {
	m := testManager(t)
	addNode(t, m, "small", map[string]string{"disk": "hdd"}, "1", nil)
	addNode(t, m, "ssd", map[string]string{"disk": "ssd"}, "4", nil)
	addNode(t, m, "gpu", map[string]string{"disk": "ssd"}, "8", func(n *apis.Node) {
		n.Spec.Taints = []apis.Taint{{Key: "gpu", Value: "true", Effect: "NoSchedule"}}
	})
	addNode(t, m, "cordoned", map[string]string{"disk": "ssd"}, "8", func(n *apis.Node) { n.Spec.Unschedulable = true })

	create(t, m, podResource, newPod("selector", func(p *apis.Pod) { p.Spec.NodeSelector = map[string]string{"disk": "ssd"} }))
	create(t, m, podResource, newPod("tolerates", func(p *apis.Pod) {
		p.Spec.Tolerations = []apis.Toleration{{Key: "gpu", Operator: "Exists", Effect: "NoSchedule"}}
		p.Spec.Affinity = &apis.Affinity{NodeAffinity: &apis.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &apis.NodeSelector{
			NodeSelectorTerms: []apis.NodeSelectorTerm{{MatchFields: []apis.NodeSelectorRequirement{{Key: "metadata.name", Operator: "In", Values: []string{"gpu"}}}}}}}}
	}))
	create(t, m, podResource, newPod("huge", func(p *apis.Pod) {
		p.Spec.Containers[0].Resources.Requests = map[string]apis.Quantity{"cpu": "6"}
	}))
	sync(t, m, 1)

	if got := podByName(t, m, "selector").Spec.NodeName; got != "ssd" {
		t.Fatalf("nodeSelector disk=ssd went to %q", got)
	}
	if got := podByName(t, m, "tolerates").Spec.NodeName; got != "gpu" {
		t.Fatalf("matchFields + toleration went to %q", got)
	}
	huge := podByName(t, m, "huge")
	c := condition(huge, "PodScheduled")
	want := "0/4 nodes are available: 1 node(s) had untolerated taint {gpu: true}, 1 node(s) were unschedulable, 2 Insufficient cpu."
	if huge.Spec.NodeName != "" || c.Status != "False" || c.Reason != "Unschedulable" || c.Message != want {
		t.Fatalf("huge: node %q condition %+v\nwant %q", huge.Spec.NodeName, c, want)
	}
	// An unchanged reason is not written again.
	rec, _ := m.Store.Get(objects.Key(podResource, "default", "huge"))
	sync(t, m, 2)
	if again, _ := m.Store.Get(objects.Key(podResource, "default", "huge")); again.Rev != rec.Rev {
		t.Fatal("the scheduler rewrote an unschedulable Pod with the same reason")
	}
}

func TestNodeLifecycleEvictsFromASilentNode(t *testing.T) {
	m := testManager(t)
	m.NodeName = "cp"
	silence := map[string]time.Duration{}
	m.Silence = func(n string) time.Duration { return silence[n] }
	addNode(t, m, "w1", nil, "4", nil)
	tolerant := newPod("tolerant", func(p *apis.Pod) {
		p.Spec.NodeName = "w1"
		p.Spec.Tolerations = []apis.Toleration{{Key: UnreachableTaint, Operator: "Exists", Effect: "NoExecute"}}
	})
	plain := newPod("plain", func(p *apis.Pod) { p.Spec.NodeName = "w1" })
	for _, p := range []*apis.Pod{tolerant, plain} {
		create(t, m, podResource, p) // creating resets status; the node agent reports it
		p.Status = apis.PodStatus{Phase: "Running", Conditions: []apis.PodCondition{{Type: "Ready", Status: "True"}}}
		if err := m.Store.Write(false, func(tx *store.Tx) error {
			rec, err := tx.Get(objects.Key(podResource, "default", p.Metadata.Name))
			if err != nil {
				return err
			}
			_, _, err = tx.Update(objects.Put(podResource, p), rec.Rev)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	sync(t, m, 1)
	if p := podByName(t, m, "plain"); condition(p, "Ready").Status != "True" {
		t.Fatal("a live node's Pod lost Ready")
	}

	silence["w1"] = time.Minute
	sync(t, m, 1)
	rec, _ := m.Store.Get(store.Key{Resource: "nodes", Name: "w1"})
	var n apis.Node
	json.Unmarshal(rec.Data, &n)
	if len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Key != UnreachableTaint || n.Status.Conditions[0].Status != "Unknown" {
		t.Fatalf("silent node: taints %+v conditions %+v", n.Spec.Taints, n.Status.Conditions)
	}
	for _, name := range []string{"plain", "tolerant"} {
		p := podByName(t, m, name)
		if condition(p, "Ready").Status != "False" || p.Metadata.DeletionTimestamp != nil {
			t.Fatalf("%s: before the toleration time it must leave endpoints but stay: %+v", name, p.Status.Conditions)
		}
	}

	// Pretend the taint was added more than five minutes ago.
	n.Spec.Taints[0].TimeAdded = &apis.Time{Time: time.Now().Add(-6 * time.Minute)}
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		_, _, err := tx.Update(objects.Put(nodeResource, &n), rec.Rev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sync(t, m, 1)
	if p := podByName(t, m, "plain"); p.Metadata.DeletionTimestamp == nil {
		t.Fatal("a Pod without a toleration was not evicted after five minutes")
	}
	if p := podByName(t, m, "tolerant"); p.Metadata.DeletionTimestamp != nil {
		t.Fatal("a Pod that tolerates the taint forever was evicted")
	}

	// The node reports again: the taint goes. (A pass in which garbage
	// collection changed something ends early, so give it two.)
	silence["w1"] = 0
	sync(t, m, 2)
	rec, _ = m.Store.Get(store.Key{Resource: "nodes", Name: "w1"})
	n = apis.Node{} // Unmarshal leaves fields the JSON omits as they were
	json.Unmarshal(rec.Data, &n)
	if len(n.Spec.Taints) != 0 {
		t.Fatalf("taint stayed after the node came back: %+v", n.Spec.Taints)
	}

	// The node is removed: its Pods go at once.
	if err := m.Store.Write(false, func(tx *store.Tx) error {
		_, err := tx.Delete(rec.Key, rec.Rev, objects.Encoder(&n))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sync(t, m, 2)
	for _, p := range pods(t, m) {
		if strings.HasPrefix(p.Spec.NodeName, "w1") {
			t.Fatalf("Pod %s of a removed node is still there", p.Metadata.Name)
		}
	}
}
