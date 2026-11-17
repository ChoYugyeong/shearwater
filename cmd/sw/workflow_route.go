package main

// Service는 앱 이름과 다를 수 있고 라벨 이름도 사용자가 정한다. 따라서
// 특정 라벨 철자를 요구하지 않는다. 연결할 때 실제 경로를 저장하고, 매번
// Service UID → selector → Pod → ReplicaSet → Deployment UID를 확인한다.
// 이것은 관리자 토큰을 제한하는 RBAC가 아니라 실수로 다른 앱을 고르거나
// 다른 앱의 HTTP 성공을 이 앱의 성공으로 보고하는 것을 막는 검사다.
import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"strconv"
	"time"

	"shearwater/internal/agent"
	"shearwater/internal/apis"
)

type workflowRoute struct {
	Name     string            `json:"name"`
	IP       string            `json:"clusterIP"`
	Selector map[string]string `json:"selector"`
	Port     apis.ServicePort  `json:"port"`
}

func (w appWorkflow) serviceName() string {
	if w.Route != nil {
		return w.Route.Name
	}
	return w.Name // alpha.10 이전 파일에는 기존의 좁은 검사 조건을 유지한다.
}

func (w appWorkflow) servicePath() string {
	return "/api/v1/namespaces/" + w.Namespace + "/services/" + w.serviceName()
}

func routeMatches(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if actual, exists := labels[k]; !exists || actual != v {
			return false
		}
	}
	return true
}

func routeFromService(s apis.Service, port int32) (*workflowRoute, error) {
	ip, err := netip.ParseAddr(s.Spec.ClusterIP)
	if err != nil || !apis.ServiceCIDR.Contains(ip) || s.Spec.Type != "ClusterIP" || len(s.Spec.Selector) == 0 || s.Metadata.DeletionTimestamp != nil {
		return nil, fmt.Errorf("Service %s needs an active IPv4 ClusterIP and a nonempty selector", s.Metadata.Name)
	}
	var selected []apis.ServicePort
	for _, p := range s.Spec.Ports {
		if p.Protocol == "TCP" && (port == 0 || p.Port == port) {
			selected = append(selected, p)
		}
	}
	if len(selected) != 1 {
		return nil, fmt.Errorf("Service %s needs one selected TCP port; use --port with an actual Service port", s.Metadata.Name)
	}
	return &workflowRoute{Name: s.Metadata.Name, IP: ip.String(), Selector: maps.Clone(s.Spec.Selector), Port: selected[0]}, nil
}

// Service의 port는 방문자가 쓰는 포트, targetPort는 컨테이너가 듣는 포트다.
// 숫자 targetPort는 containerPort 선언 없이도 유효하다. 이름이면 실제
// 컨테이너의 이름 붙은 TCP 포트를 찾아야 한다. 둘을 같은 숫자로 가정하지 않는다.
func routeTarget(p apis.ServicePort, spec apis.PodSpec) (int32, error) {
	if !p.TargetPort.IsString {
		n := p.TargetPort.Int
		if n == 0 {
			n = p.Port
		}
		if n > 0 && n <= 65535 {
			return n, nil
		}
	} else {
		for _, c := range spec.Containers {
			for _, cp := range c.Ports {
				if cp.Name == p.TargetPort.Str && (cp.Protocol == "TCP" || cp.Protocol == "") {
					return cp.ContainerPort, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("selected targetPort does not resolve to a container TCP port")
}

func routeOwner(meta apis.ObjectMeta, kind, name, uid string) bool {
	for _, o := range meta.OwnerReferences {
		if o.Kind == kind && o.Name == name && o.UID == uid && uid != "" && o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

func validateRouteService(w appWorkflow, s apis.Service, d apis.Deployment) (*workflowRoute, error) {
	if s.Kind != "Service" || s.Metadata.Namespace != w.Namespace || s.Metadata.Name != w.serviceName() || s.Metadata.UID == "" || s.Metadata.UID != w.ServiceUID {
		return nil, fmt.Errorf("STALE: connected Service was removed or replaced; reconnect explicitly")
	}
	r, err := routeFromService(s, w.Port)
	if err != nil {
		return nil, err
	}
	if w.Route != nil {
		if r.Name != w.Route.Name || r.IP != w.Route.IP || r.Port != w.Route.Port || !maps.Equal(r.Selector, w.Route.Selector) {
			return nil, fmt.Errorf("STALE: connected Service route changed; review it and reconnect explicitly")
		}
	} else if len(s.Spec.Ports) != 1 || len(r.Selector) != 1 || r.Selector["app.kubernetes.io/name"] != w.Name || r.Port.TargetPort.IsString || r.Port.TargetPort.Int != w.Port {
		return nil, fmt.Errorf("legacy workflow has no route snapshot; run sw connect to create a new workflow for this Service")
	}
	if d.Metadata.UID != w.UID || d.Metadata.Namespace != w.Namespace || !routeMatches(r.Selector, d.Spec.Template.Metadata.Labels) {
		return nil, fmt.Errorf("connected Service no longer selects the connected Deployment")
	}
	if _, err = routeTarget(r.Port, d.Spec.Template.Spec); err != nil {
		return nil, err
	}
	return r, nil
}

// 같은 라벨의 별도 앱이 섞이면 HTTP 200도 어느 앱에서 왔는지 알 수 없다.
// Pod 이름/라벨만 믿지 않고 controller UID 사슬을 확인한다. 롤링 중인 옛
// ReplicaSet과 새 ReplicaSet은 둘 다 같은 Deployment 소유이므로 허용된다.
func validateRouteBackends(w appWorkflow, r workflowRoute, s apis.Service, snapshot agent.Snapshot, slices []apis.EndpointSlice) (bool, error) {
	sets := map[string]string{}
	for _, rs := range snapshot.ReplicaSets {
		if rs.Metadata.Namespace == w.Namespace && routeOwner(rs.Metadata, "Deployment", w.Name, w.UID) {
			sets[rs.Metadata.UID] = rs.Metadata.Name
		}
	}
	pods := map[string]apis.Pod{}
	for _, p := range snapshot.Pods {
		if p.Metadata.Namespace != w.Namespace || !routeMatches(r.Selector, p.Metadata.Labels) {
			continue
		}
		owned := false
		for uid, name := range sets {
			owned = owned || routeOwner(p.Metadata, "ReplicaSet", name, uid)
		}
		if !owned || p.Metadata.UID == "" {
			return false, fmt.Errorf("ambiguous Service: a selected Pod is not owned by the connected Deployment")
		}
		pods[p.Metadata.UID] = p
	}
	ready := false
	for _, sl := range slices {
		if sl.Metadata.Namespace != w.Namespace || sl.Metadata.Labels[apis.ServiceNameLabel] != s.Metadata.Name {
			continue
		}
		if !routeOwner(sl.Metadata, "Service", s.Metadata.Name, s.Metadata.UID) || sl.AddressType != "IPv4" {
			return false, fmt.Errorf("Service EndpointSlice ownership or address family mismatch")
		}
		if g := sl.Metadata.Annotations[apis.ServiceGenerationAnnotation]; g != "" && g != strconv.FormatInt(s.Metadata.Generation, 10) {
			continue
		}
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready == nil || !*ep.Conditions.Ready || ep.Conditions.Terminating != nil && *ep.Conditions.Terminating {
				continue
			}
			ref := ep.TargetRef
			if ref == nil {
				return false, fmt.Errorf("Service endpoint has no Pod identity")
			}
			p, ok := pods[ref.UID]
			if !ok || ref.Kind != "Pod" || ref.Namespace != w.Namespace || ref.Name != p.Metadata.Name || p.Metadata.DeletionTimestamp != nil {
				return false, fmt.Errorf("Service endpoint does not belong to the connected app")
			}
			target, err := routeTarget(r.Port, p.Spec)
			if err != nil {
				return false, err
			}
			portMatches := false
			for _, port := range sl.Ports {
				name := ""
				if port.Name != nil {
					name = *port.Name
				}
				if name == r.Port.Name && port.Protocol != nil && *port.Protocol == "TCP" && port.Port != nil && *port.Port == target {
					portMatches = true
				}
			}
			if !portMatches {
				continue
			}
			for _, address := range ep.Addresses {
				if address != p.Status.PodIP || address == "" {
					return false, fmt.Errorf("Service endpoint IP differs from its owned Pod")
				}
				ready = true
			}
		}
	}
	return ready, nil
}

// 여러 GET은 하나의 트랜잭션이 아니다. 같은 DB 시점의 Pod와 EndpointSlice를
// 읽고 Service도 전후 비교한다. 변화가 계속되면 성공을 추측하지 않고 다시
// 관찰하라고 알린다. 읽기만 최대 3번 반복하고 쓰기는 재전송하지 않는다.
func checkedWorkflowRoute(ctx context.Context, c *agentClient, w appWorkflow) (apis.Service, bool, error) {
	for attempt := 0; attempt < 3; attempt++ {
		var svc, after apis.Service
		if err := c.get(ctx, w.servicePath(), &svc); err != nil {
			return svc, false, err
		}
		snapshot, err := readFocusEvidence(ctx, c.get, w.focus(c, w.Goal.Generation))
		if err != nil {
			return svc, false, err
		}
		if len(snapshot.Deployments) != 1 {
			return svc, false, fmt.Errorf("connected Deployment is missing")
		}
		r, err := validateRouteService(w, svc, snapshot.Deployments[0])
		if err != nil {
			return svc, false, err
		}
		var slices inspectList[apis.EndpointSlice]
		if err = c.get(ctx, "/apis/discovery.k8s.io/v1/namespaces/"+w.Namespace+"/endpointslices", &slices); err != nil {
			return svc, false, err
		}
		if err = c.get(ctx, w.servicePath(), &after); err != nil {
			return svc, false, err
		}
		if slices.revision() == snapshot.Revision && svc.Metadata.ResourceVersion == after.Metadata.ResourceVersion && svc.Metadata.UID == after.Metadata.UID {
			ready, err := validateRouteBackends(w, *r, svc, snapshot, slices.Items)
			return svc, ready, err
		}
		select {
		case <-ctx.Done():
			return svc, false, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return apis.Service{}, false, fmt.Errorf("CONFLICT: Service route kept changing; retry observation")
}

func connectExistingWorkflow(ctx context.Context, c *agentClient, w *appWorkflow, d *apis.Deployment, service string, port int32) error {
	if len(d.Spec.Template.Spec.Containers) != 1 {
		return fmt.Errorf("workflow requires one app container; use focused tools for a multi-container app")
	}
	currentImage := d.Spec.Template.Spec.Containers[0].Image
	if w.Image != "" && w.Image != currentImage {
		return fmt.Errorf("--image differs from the existing app; connect without --image, then prepare an update")
	}
	w.Image = currentImage
	var services inspectList[apis.Service]
	if err := c.get(ctx, "/api/v1/namespaces/"+w.Namespace+"/services", &services); err != nil {
		return err
	}
	var candidates []apis.Service
	for _, s := range services.Items {
		if s.Metadata.Namespace == w.Namespace && (service == "" || service == s.Metadata.Name) && routeMatches(s.Spec.Selector, d.Spec.Template.Metadata.Labels) {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) != 1 {
		return fmt.Errorf("found %d matching Services; select exactly one with --service NAME (the selector must match the app)", len(candidates))
	}
	svc := candidates[0]
	r, err := routeFromService(svc, port)
	if err != nil {
		return err
	}
	w.UID, w.ServiceUID, w.Route, w.Port = d.Metadata.UID, svc.Metadata.UID, r, r.Port.Port
	w.Goal = agent.ReadinessGoal{Namespace: w.Namespace, Resource: "deploy/" + w.Name, UID: w.UID, Generation: d.Metadata.Generation}
	_, _, err = checkedWorkflowRoute(ctx, c, *w)
	return err
}
