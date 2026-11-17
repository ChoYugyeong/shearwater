package main

// 앱 이름은 Pod 이름이 아니다. 모델에게 그 이름과 namespace를 매번 다시
// 쓰게 하지 않고, 선택한 Deployment → ReplicaSet → Pod의 소유 UID를 따라간다.
// 같은 라벨을 붙인 다른 앱이나 같은 이름으로 재생성된 객체를 섞지 않는다.
import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"shearwater/internal/agent"
	"shearwater/internal/apis"
)

type focusContainerEvidence struct {
	Name      string `json:"name"`
	Image     string `json:"image"`
	Ready     bool   `json:"ready"`
	State     string `json:"state"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
	Restarts  int32  `json:"restarts"`
	ExitCode  *int32 `json:"exitCode,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type focusPodEvidence struct {
	Name              string                   `json:"name"`
	Phase             string                   `json:"phase"`
	Ready             bool                     `json:"ready"`
	Terminating       bool                     `json:"terminating"`
	Containers        []focusContainerEvidence `json:"containers"`
	OmittedContainers int                      `json:"omittedContainers"`
}

type focusDiagnosis struct {
	focusStatus
	Revision    string             `json:"revision"`
	OwnedPods   int                `json:"ownedPods"`
	ReadyPods   int                `json:"readyPods"`
	OmittedPods int                `json:"omittedPods"`
	Pods        []focusPodEvidence `json:"pods"`
	Limits      []string           `json:"limits"`
}

func diagnoseFocus(ctx context.Context, s *focusSession) (string, error) {
	snapshot, err := readFocusEvidence(ctx, s.client.get, s)
	if err != nil {
		return "", err
	}
	v, err := snapshot.Verify(s.goal, time.Now())
	if err != nil {
		return "", err
	}
	status := focusStatus{State: v.State, HTTP: "not_checked"}
	if len(snapshot.Deployments) == 1 {
		d := snapshot.Deployments[0]
		status.UID, status.Generation = d.Metadata.UID, d.Metadata.Generation
	}
	// 다른 UID/세대의 Pod 근거를 원래 앱의 진단으로 내보내지 않는다.
	if status.UID != s.goal.UID || status.Generation != s.goal.Generation {
		return finishFocusAnswer(s, status, v.Reason)
	}
	return finishFocusAnswer(s, focusEvidence(snapshot, s, status), v.Reason)
}

func readFocusEvidence(ctx context.Context, get inspectGET, s *focusSession) (agent.Snapshot, error) {
	for attempt := range inspectAttempts {
		var deployments inspectList[apis.Deployment]
		var sets inspectList[apis.ReplicaSet]
		var pods inspectList[apis.Pod]
		sources := []struct {
			path string
			list interface{ revision() string }
		}{
			{"/apis/apps/v1/namespaces/" + s.Namespace + "/deployments?fieldSelector=" + url.QueryEscape("metadata.name="+strings.TrimPrefix(s.Target, "deploy/")), &deployments},
			{"/apis/apps/v1/namespaces/" + s.Namespace + "/replicasets", &sets},
			{"/api/v1/namespaces/" + s.Namespace + "/pods", &pods},
		}
		// API의 목록 revision은 SQLite 전체의 같은 시점을 뜻한다. 상태가
		// 바뀌는 중이면 섞인 근거를 출력하지 않고 짧게 다시 읽거나 실패한다.
		revision, matched := "", true
		for _, src := range sources {
			if err := get(ctx, src.path, src.list); err != nil {
				return agent.Snapshot{}, err
			}
			got := src.list.revision()
			if _, err := strconv.ParseUint(got, 10, 63); err != nil {
				return agent.Snapshot{}, fmt.Errorf("invalid diagnosis list revision")
			}
			if revision != "" && revision != got {
				matched = false
				break
			}
			revision = got
		}
		if matched {
			if len(deployments.Items) > 1 {
				return agent.Snapshot{}, fmt.Errorf("diagnosis target selection mismatch")
			}
			for _, d := range deployments.Items {
				if d.Kind != "Deployment" || d.Metadata.Namespace != s.Namespace || "deploy/"+d.Metadata.Name != s.Target || d.Metadata.UID == "" || d.Metadata.Generation < 1 {
					return agent.Snapshot{}, fmt.Errorf("diagnosis target identity missing or mismatched")
				}
			}
			return agent.Snapshot{Revision: revision, Deployments: deployments.Items, ReplicaSets: sets.Items, Pods: pods.Items}, nil
		}
		if attempt+1 < inspectAttempts {
			select {
			case <-ctx.Done():
				return agent.Snapshot{}, ctx.Err()
			case <-time.After(20 * time.Millisecond << attempt):
			}
		}
	}
	return agent.Snapshot{}, fmt.Errorf("CONFLICT: diagnosis revision kept changing; retry observation")
}

func focusEvidence(snapshot agent.Snapshot, s *focusSession, status focusStatus) focusDiagnosis {
	r := focusDiagnosis{focusStatus: status, Revision: snapshot.Revision, Pods: []focusPodEvidence{},
		Limits: []string{"API evidence only; HTTP not checked", "at most 4 Pods and 3 containers each; failures first", "progressDeadlineSeconds and minReadySeconds are stored, not enforced", "recovery needs a new reviewed and approved plan"}}
	ownedSets := map[string]bool{}
	for _, rs := range snapshot.ReplicaSets {
		if rs.Metadata.Namespace != s.Namespace || rs.Metadata.UID == "" {
			continue
		}
		for _, owner := range rs.Metadata.OwnerReferences {
			if owner.Kind == "Deployment" && owner.UID == s.goal.UID && owner.Controller != nil && *owner.Controller {
				ownedSets[rs.Metadata.UID] = true
			}
		}
	}
	var evidence []focusPodEvidence
	for _, p := range snapshot.Pods {
		owned := false
		for _, owner := range p.Metadata.OwnerReferences {
			owned = owned || owner.Kind == "ReplicaSet" && ownedSets[owner.UID] && owner.Controller != nil && *owner.Controller
		}
		if !owned || p.Metadata.Namespace != s.Namespace {
			continue
		}
		e := focusPodEvidence{Name: p.Metadata.Name, Phase: p.Status.Phase, Terminating: p.Metadata.DeletionTimestamp != nil, Containers: []focusContainerEvidence{}}
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" && p.Status.Phase == "Running" && !e.Terminating {
				e.Ready = true
			}
		}
		r.OwnedPods++
		if e.Ready {
			r.ReadyPods++
		}
		for _, spec := range p.Spec.Containers {
			c := focusContainerEvidence{Name: spec.Name, Image: spec.Image, State: "not_reported"}
			for _, cs := range p.Status.ContainerStatuses {
				if cs.Name != spec.Name {
					continue
				}
				c.Ready, c.Restarts = cs.Ready, cs.RestartCount
				if w := cs.State.Waiting; w != nil {
					c.State, c.Reason, c.Message = "waiting", w.Reason, w.Message
				} else if t := cs.State.Terminated; t != nil {
					c.State, c.Reason, c.ExitCode = "terminated", t.Reason, &t.ExitCode
				} else if cs.State.Running != nil {
					c.State = "running"
				}
			}
			// 로그·환경 변수는 제외한다. 오류 문장도 신뢰할 명령이 아닌 데이터다.
			for _, field := range []*string{&c.Name, &c.Image, &c.Reason, &c.Message} {
				if len(*field) > 160 {
					value := (*field)[:160]
					if field == &c.Message {
						// 런타임 오류는 긴 이미지 이름을 반복한 뒤 맨 끝에
						// not found 같은 직접 원인을 쓴다. 앞·뒤를 함께 보존한다.
						value = (*field)[:96]
						for !utf8.ValidString(value) {
							value = value[:len(value)-1]
						}
						tail := (*field)[len(*field)-61:]
						for !utf8.ValidString(tail) {
							tail = tail[1:]
						}
						value += "..." + tail
					}
					for !utf8.ValidString(value) {
						value = value[:len(value)-1]
					}
					*field, c.Truncated = value, true
				}
			}
			e.Containers = append(e.Containers, c)
		}
		// 실패 컨테이너를 먼저 보여 주고 생략을 명시한다. 잘라낸 내용을
		// 모든 컨테이너의 정상이라는 결론으로 바꾸면 안 된다.
		sort.SliceStable(e.Containers, func(i, j int) bool { return !e.Containers[i].Ready && e.Containers[j].Ready })
		if len(e.Containers) > 3 {
			e.OmittedContainers = len(e.Containers) - 3
			e.Containers = e.Containers[:3]
		}
		evidence = append(evidence, e)
	}
	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].Ready != evidence[j].Ready {
			return !evidence[i].Ready
		}
		return evidence[i].Name < evidence[j].Name
	})
	if len(evidence) > 4 {
		r.OmittedPods = len(evidence) - 4
		evidence = evidence[:4]
	}
	r.Pods = append(r.Pods, evidence...)
	// JSON은 제어 문자를 이스케이프하면서 커질 수 있다. 문자열 길이 제한만
	// 믿지 않고 직렬화 크기도 확인해 모델의 도구 응답 예산 안에 둔다.
	for len(r.Pods) > 0 {
		encoded, _ := json.Marshal(r)
		if len(encoded) <= 8<<10 {
			break
		}
		r.Pods = r.Pods[:len(r.Pods)-1]
		r.OmittedPods++
	}
	return r
}
