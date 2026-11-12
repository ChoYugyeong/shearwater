package agent

import (
	"errors"
	"strings"
	"testing"

	"shearwater/internal/apis"
)

func TestTreeProblemsKeepsAncestors(t *testing.T) {
	d := testDeployment("default", "web", "deploy-web")
	d.Metadata.Annotations = map[string]string{apis.DeploymentRevisionAnnotation: "2"}
	d.Spec.Replicas = new(int32(2))
	d.Spec.Template.Spec.Containers = []apis.Container{{Name: "web", Image: "nginx:1.27"}}
	d.Status = apis.DeploymentStatus{Replicas: 2, ReadyReplicas: 2}
	rs := apis.ReplicaSet{
		TypeMeta: apis.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"},
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "web-current", UID: "rs-current",
			OwnerReferences: []apis.OwnerReference{testOwner(&d, true)}, Annotations: map[string]string{apis.DeploymentRevisionAnnotation: "2"}},
		Spec: apis.ReplicaSetSpec{Replicas: new(int32(2))}, Status: apis.ReplicaSetStatus{Replicas: 2, ReadyReplicas: 2},
	}
	old := rs
	old.Metadata.Name, old.Metadata.UID = "web-old", "rs-old"
	old.Metadata.Annotations = map[string]string{apis.DeploymentRevisionAnnotation: "1"}
	old.Spec.Replicas, old.Status = new(int32), apis.ReplicaSetStatus{}
	missing := rs
	missing.Metadata.Name, missing.Metadata.UID = "lonely", "rs-lonely"
	missing.Metadata.OwnerReferences = nil
	missing.Status = apis.ReplicaSetStatus{Replicas: 1, ReadyReplicas: 1}

	healthy := testRunningPod("default", "web-good")
	healthy.Metadata.OwnerReferences = []apis.OwnerReference{testOwner(&rs, true)}
	waiting := testRunningPod("default", "web-bad")
	waiting.Metadata.OwnerReferences = []apis.OwnerReference{testOwner(&rs, true)}
	waiting.Status = apis.PodStatus{Phase: "Pending", ContainerStatuses: []apis.ContainerStatus{{Name: "web",
		State: apis.ContainerState{Waiting: &apis.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}
	done := testRunningPod("default", "done")
	done.Status = apis.PodStatus{Phase: "Succeeded"}
	failed := testRunningPod("default", "failed")
	failed.Status = apis.PodStatus{Phase: "Failed", ContainerStatuses: []apis.ContainerStatus{{Name: "web", RestartCount: 2,
		State: apis.ContainerState{Terminated: &apis.ContainerStateTerminated{Reason: "Error", ExitCode: 17}}}}}
	unscheduled := testRunningPod("default", "unscheduled")
	unscheduled.Status = apis.PodStatus{Phase: "Pending", Conditions: []apis.PodCondition{{Type: "PodScheduled", Status: "False", Reason: "Unschedulable"}}}
	terminating := testRunningPod("default", "terminating")
	terminating.Metadata.DeletionTimestamp = apis.Now()

	s := Snapshot{Revision: "1042", Nodes: []apis.Node{{Metadata: apis.ObjectMeta{Name: "sw-node"},
		Status: apis.NodeStatus{Conditions: []apis.NodeCondition{{Type: "Ready", Status: "True"}}}}},
		Namespaces:  []apis.Namespace{{Metadata: apis.ObjectMeta{Name: "kube-system"}}, {Metadata: apis.ObjectMeta{Name: "default"}}},
		Deployments: []apis.Deployment{d}, ReplicaSets: []apis.ReplicaSet{old, rs, missing},
		Pods: []apis.Pod{healthy, waiting, done, failed, unscheduled, terminating},
	}
	want := `cluster shearwater rev=1042 node=sw-node Ready pods=1/6
ns default
  deploy/web 2/2 ready rev=2 image=nginx:1.27
    rs/web-current 2/2
      pod/web-bad Pending 0/1 ImagePullBackOff restarts=0
      pod/web-good Running 1/1 restarts=0 10.244.0.5
    rs/web-old 0/0 old
  pod/done Succeeded 0/1 restarts=0
  pod/failed Failed 0/1 Error exit=17 restarts=2
  pod/terminating Running 1/1 restarts=0 10.244.0.5 Terminating
  pod/unscheduled Pending 0/1 Unschedulable restarts=0
  rs/lonely 1/2 missing=1
ns kube-system
`
	if got := mustTree(t, s, TreeOptions{}); got != want {
		t.Fatalf("full tree:\n%s\nwant:\n%s", got, want)
	}
	want = `cluster shearwater rev=1042 node=sw-node Ready pods=1/6
ns default
  deploy/web 2/2 ready rev=2 image=nginx:1.27
    rs/web-current 2/2
      pod/web-bad Pending 0/1 ImagePullBackOff restarts=0
  pod/failed Failed 0/1 Error exit=17 restarts=2
  pod/terminating Running 1/1 restarts=0 10.244.0.5 Terminating
  pod/unscheduled Pending 0/1 Unschedulable restarts=0
  rs/lonely 1/2 missing=1
`
	if got := mustTree(t, s, TreeOptions{Problems: true}); got != want {
		t.Fatalf("problems tree:\n%s\nwant:\n%s", got, want)
	}
}

func TestTreeRootAndOwnerUID(t *testing.T) {
	d := testDeployment("default", "web", "d-current")
	foreign := testDeployment("other", "web", "d-other")
	fallback := testDeployment("default", "fallback", "d-fallback")
	child := testRunningPod("default", "child")
	child.Metadata.OwnerReferences = []apis.OwnerReference{testOwner(&fallback, false), testOwner(&d, true)}
	stale := testRunningPod("default", "stale")
	stale.Metadata.OwnerReferences = []apis.OwnerReference{testOwner(&d, true)}
	stale.Metadata.OwnerReferences[0].UID = "d-removed"
	cross := testRunningPod("default", "cross")
	cross.Metadata.OwnerReferences = []apis.OwnerReference{testOwner(&foreign, true)}
	s := Snapshot{Revision: "33", Deployments: []apis.Deployment{foreign, fallback, d}, Pods: []apis.Pod{stale, cross, child}}
	if _, err := s.Tree(TreeOptions{Root: "deploy/web"}); !errors.Is(err, ErrTreeAmbiguous) {
		t.Fatalf("ambiguous root = %v", err)
	}
	if _, err := s.Tree(TreeOptions{Namespace: "missing"}); !errors.Is(err, ErrTreeNotFound) {
		t.Fatalf("missing namespace = %v", err)
	}
	if _, err := s.Tree(TreeOptions{Namespace: "default", Root: "deploy/missing"}); !errors.Is(err, ErrTreeNotFound) {
		t.Fatalf("missing root = %v", err)
	}
	full := mustTree(t, s, TreeOptions{})
	for _, line := range []string{"\n    pod/child ", "\n  pod/stale ", "\n  pod/cross "} {
		if !strings.Contains(full, line) {
			t.Fatalf("missing %q in:\n%s", line, full)
		}
	}
	options := TreeOptions{Namespace: "default", Root: "deploy/web"}
	want := `cluster shearwater rev=33 node=<none> pods=3/3
ns default
  deploy/web 0/0 ready
    pod/child Running 1/1 restarts=0 10.244.0.5
`
	if got := mustTree(t, s, options); got != want {
		t.Fatalf("root tree:\n%s\nwant:\n%s", got, want)
	}
	options.Problems = true
	if got := mustTree(t, s, options); got != s.Summary()+"\n" {
		t.Fatalf("healthy root with --problems:\n%s", got)
	}
}

func TestTreeCyclicOwnerReferences(t *testing.T) {
	a, b := testRunningPod("default", "a"), testRunningPod("default", "b")
	a.Metadata.OwnerReferences = []apis.OwnerReference{testOwner(&b, true)}
	b.Metadata.OwnerReferences = []apis.OwnerReference{testOwner(&a, true)}
	s := Snapshot{Revision: "34", Pods: []apis.Pod{b, a}}
	got := mustTree(t, s, TreeOptions{})
	if strings.Count(got, "pod/a ") != 1 || strings.Count(got, "pod/b ") != 1 {
		t.Fatalf("cyclic owners hid or duplicated Pods:\n%s", got)
	}
}

func TestTreeServiceEndpointCounts(t *testing.T) {
	svc := apis.Service{TypeMeta: apis.TypeMeta{Kind: "Service", APIVersion: "v1"},
		Metadata: apis.ObjectMeta{Namespace: "default", Name: "web", UID: "svc-current", Generation: 3},
		Spec:     apis.ServiceSpec{ClusterIP: "10.96.0.12", Selector: map[string]string{"app": "web"}, Ports: []apis.ServicePort{{Port: 443}, {Port: 80}}}}
	other := svc
	other.Metadata.Name, other.Metadata.UID, other.Spec.ClusterIP = "empty", "svc-empty", "10.96.0.13"
	headless := other
	headless.Metadata.Name, headless.Metadata.UID, headless.Spec.ClusterIP = "headless", "svc-headless", "None"
	ready := func(ip string) apis.Endpoint {
		return apis.Endpoint{Addresses: []string{ip}, Conditions: apis.EndpointConditions{Ready: new(true)}}
	}
	unready := ready("10.244.0.7")
	unready.Conditions.Ready = new(false)
	terminating := ready("10.244.0.8")
	terminating.Conditions.Terminating = new(true)
	slice := apis.EndpointSlice{Metadata: apis.ObjectMeta{Namespace: "default", Name: "web-one",
		Labels: map[string]string{apis.ServiceNameLabel: "web"}, OwnerReferences: []apis.OwnerReference{testOwner(&svc, true)},
		Annotations: map[string]string{apis.ServiceGenerationAnnotation: "3"}},
		Endpoints: []apis.Endpoint{ready("10.244.0.5"), unready, terminating}}
	second := slice
	second.Endpoints = []apis.Endpoint{ready("10.244.0.5"), ready("10.244.0.6")}
	oldSpec := slice
	oldSpec.Metadata.Annotations = map[string]string{apis.ServiceGenerationAnnotation: "2"}
	oldSpec.Endpoints = []apis.Endpoint{ready("10.244.0.9")}
	oldService := slice
	oldService.Metadata.OwnerReferences = []apis.OwnerReference{testOwner(&svc, true)}
	oldService.Metadata.OwnerReferences[0].UID = "svc-removed"
	oldService.Endpoints = []apis.Endpoint{ready("10.244.0.10")}

	s := Snapshot{Revision: "40", Services: []apis.Service{headless, svc, other}, EndpointSlices: []apis.EndpointSlice{oldService, second, oldSpec, slice}}
	full := mustTree(t, s, TreeOptions{})
	if !strings.Contains(full, "  svc/web 10.96.0.12:80,443 -> 2 endpoints\n") {
		t.Fatalf("wrong endpoint count:\n%s", full)
	}
	want := s.Summary() + "\nns default\n  svc/empty 10.96.0.13:80,443 -> 0 endpoints NoEndpoints\n"
	if got := mustTree(t, s, TreeOptions{Problems: true}); got != want {
		t.Fatalf("service problems:\n%s\nwant:\n%s", got, want)
	}
}

func mustTree(t *testing.T, s Snapshot, options TreeOptions) string {
	t.Helper()
	got, err := s.Tree(options)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func testDeployment(namespace, name, uid string) apis.Deployment {
	return apis.Deployment{TypeMeta: apis.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		Metadata: apis.ObjectMeta{Namespace: namespace, Name: name, UID: uid}, Spec: apis.DeploymentSpec{Replicas: new(int32)}}
}

func testRunningPod(namespace, name string) apis.Pod {
	return apis.Pod{TypeMeta: apis.TypeMeta{APIVersion: "v1", Kind: "Pod"}, Metadata: apis.ObjectMeta{Namespace: namespace, Name: name, UID: "pod-" + name},
		Spec: apis.PodSpec{Containers: []apis.Container{{Name: "web", Image: "nginx:1.27"}}},
		Status: apis.PodStatus{Phase: "Running", PodIP: "10.244.0.5", Conditions: []apis.PodCondition{{Type: "Ready", Status: "True"}},
			ContainerStatuses: []apis.ContainerStatus{{Name: "web", Ready: true, State: apis.ContainerState{Running: &apis.ContainerStateRunning{}}}}}}
}

func testOwner(obj apis.Object, controller bool) apis.OwnerReference {
	return apis.OwnerReference{APIVersion: obj.Type().APIVersion, Kind: obj.Type().Kind, Name: obj.Meta().Name, UID: obj.Meta().UID, Controller: &controller}
}
