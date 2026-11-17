package main

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"shearwater/internal/agent"
	"shearwater/internal/apis"
)

func routeFixture() (appWorkflow, apis.Service, agent.Snapshot, []apis.EndpointSlice) {
	yes := true
	owner := func(kind, name, uid string) []apis.OwnerReference {
		return []apis.OwnerReference{{Kind: kind, Name: name, UID: uid, Controller: &yes}}
	}
	labels := map[string]string{"app": "old-web", "tier": "frontend"}
	port := apis.ServicePort{Name: "web", Protocol: "TCP", Port: 8080, TargetPort: apis.IntOrString{IsString: true, Str: "http"}}
	w := appWorkflow{Name: "web", Namespace: "default", UID: "dep-1", ServiceUID: "svc-1", Port: 8080, Route: &workflowRoute{Name: "public-web", IP: "10.96.0.10", Selector: labels, Port: port}}
	d := apis.Deployment{Metadata: apis.ObjectMeta{Name: w.Name, Namespace: w.Namespace, UID: w.UID}}
	d.Spec.Template.Metadata.Labels = labels
	d.Spec.Template.Spec.Containers = []apis.Container{{Name: "web", Image: "nginx:1.27", Ports: []apis.ContainerPort{{Name: "http", ContainerPort: 80, Protocol: "TCP"}}}}
	s := apis.Service{TypeMeta: apis.TypeMeta{Kind: "Service"}, Metadata: apis.ObjectMeta{Name: w.Route.Name, Namespace: w.Namespace, UID: w.ServiceUID, Generation: 1}, Spec: apis.ServiceSpec{Type: "ClusterIP", ClusterIP: w.Route.IP, Selector: labels, Ports: []apis.ServicePort{port}}}
	rs := apis.ReplicaSet{Metadata: apis.ObjectMeta{Name: "web-rs", Namespace: w.Namespace, UID: "rs-1", OwnerReferences: owner("Deployment", w.Name, w.UID)}}
	p := apis.Pod{Metadata: apis.ObjectMeta{Name: "web-pod", Namespace: w.Namespace, UID: "pod-1", Labels: labels, OwnerReferences: owner("ReplicaSet", "web-rs", "rs-1")}, Spec: d.Spec.Template.Spec}
	p.Status.PodIP = "10.244.0.9"
	num := int32(80)
	proto, name := "TCP", "web"
	slice := apis.EndpointSlice{Metadata: apis.ObjectMeta{Namespace: w.Namespace, Labels: map[string]string{apis.ServiceNameLabel: s.Metadata.Name}, OwnerReferences: owner("Service", s.Metadata.Name, s.Metadata.UID)}, AddressType: "IPv4", Ports: []apis.EndpointPort{{Name: &name, Protocol: &proto, Port: &num}}, Endpoints: []apis.Endpoint{{Addresses: []string{p.Status.PodIP}, Conditions: apis.EndpointConditions{Ready: &yes}, TargetRef: &apis.ObjectReference{Kind: "Pod", Name: p.Metadata.Name, Namespace: w.Namespace, UID: p.Metadata.UID}}}}
	return w, s, agent.Snapshot{Deployments: []apis.Deployment{d}, ReplicaSets: []apis.ReplicaSet{rs}, Pods: []apis.Pod{p}}, []apis.EndpointSlice{slice}
}

// 일반적인 기존 앱: 서로 다른 이름, 여러 라벨, 외부 8080→이름 http→내부80.
// 라벨 문자열을 완화한 것만으로 다른 앱이 섞이지 않는지도 함께 검사한다.
func TestWorkflowRouteOwnershipAndPortMapping(t *testing.T) {
	for _, tc := range []string{"valid", "foreign-pod", "foreign-endpoint", "wrong-ip", "wrong-port", "wrong-service-owner", "selector-changed", "replaced-service", "no-endpoints"} {
		t.Run(tc, func(t *testing.T) {
			w, s, snap, slices := routeFixture()
			switch tc {
			case "foreign-pod":
				snap.Pods[0].Metadata.OwnerReferences[0].UID = "elsewhere"
			case "foreign-endpoint":
				slices[0].Endpoints[0].TargetRef.UID = "elsewhere"
			case "wrong-ip":
				slices[0].Endpoints[0].Addresses[0] = "127.0.0.1"
			case "wrong-port":
				x := int32(9999)
				slices[0].Ports[0].Port = &x
			case "wrong-service-owner":
				slices[0].Metadata.OwnerReferences[0].UID = "elsewhere"
			case "selector-changed":
				s.Spec.Selector = map[string]string{"app": "other"}
			case "replaced-service":
				s.Metadata.UID = "elsewhere"
			case "no-endpoints":
				slices = nil
			}
			r, err := validateRouteService(w, s, snap.Deployments[0])
			ready := false
			if err == nil {
				ready, err = validateRouteBackends(w, *r, s, snap, slices)
			}
			if tc == "valid" {
				if err != nil || !ready {
					t.Fatal(ready, err)
				}
			} else if tc == "wrong-port" || tc == "no-endpoints" {
				if err != nil || ready {
					t.Fatal(ready, err)
				}
			} else if err == nil {
				t.Fatal("unsafe route accepted")
			}
		})
	}
}

func TestConnectDiscoversExistingServiceAndRejectsAmbiguity(t *testing.T) {
	c, w, o := workflowFixture(t)
	ctx := context.Background()
	if _, err := executeWorkflow(o, c, w, "deploy", ""); err != nil {
		t.Fatal(err)
	}
	approveWorkflowFixture(t, w)
	if err := executePlan(ctx, c, w.Config, w.Listen, w.Plans, w.Pending, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := w.consumeReceipt(c); err != nil {
		t.Fatal(err)
	}
	d, err := w.deployment(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	copy := *w
	copy.Image = ""
	copy.Route = nil
	if err = connectExistingWorkflow(ctx, c, &copy, d, "", 0); err != nil {
		t.Fatal(err)
	}
	if copy.Image != w.Image || copy.Route == nil || copy.ServiceUID != w.ServiceUID {
		t.Fatal(copy)
	}
	var svc apis.Service
	if err = c.get(ctx, w.servicePath(), &svc); err != nil {
		t.Fatal(err)
	}
	svc.Metadata = apis.ObjectMeta{Name: "second", Namespace: w.Namespace}
	svc.Spec.ClusterIP = ""
	body, _ := json.Marshal(svc)
	if _, _, _, err = apiRequest(ctx, c, "POST", "/api/v1/namespaces/default/services", body); err != nil {
		t.Fatal(err)
	}
	if err = connectExistingWorkflow(ctx, c, &copy, d, "", 0); err == nil {
		t.Fatal("guessed an ambiguous Service")
	}
	if err = connectExistingWorkflow(ctx, c, &copy, d, w.Name, 0); err != nil {
		t.Fatal(err)
	}
	// 승인 뒤 경로가 바뀌면 Deployment를 쓰기 전에 멈춰야 한다.
	if _, err = workflowPlan(ctx, c, &copy, "update", "nginx:1.28"); err != nil {
		t.Fatal(err)
	}
	approveWorkflowFixture(t, &copy)
	if err = c.get(ctx, w.servicePath(), &svc); err != nil {
		t.Fatal(err)
	}
	svc.Spec.Selector = map[string]string{"app": "different"}
	body, _ = json.Marshal(svc)
	if _, _, _, err = apiRequest(ctx, c, "PUT", w.servicePath(), body); err != nil {
		t.Fatal(err)
	}
	if _, err = workflowContinue(ctx, c, &copy); err == nil {
		t.Fatal("applied through changed route")
	}
	current, err := w.deployment(ctx, c)
	if err != nil || current.Spec.Template.Spec.Containers[0].Image != w.Image {
		t.Fatal("changed app before route check", err)
	}
}
