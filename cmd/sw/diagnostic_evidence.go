// 이름은 재사용될 수 있어. 그래서 이벤트는 객체의 고유 ID인 UID로 고르고,
// 로그는 읽기 전후에 UID와 컨테이너 실행 시도를 대조해. 조회 도중 앱이
// 재시작돼도 이전 상태에 새 로그를 잘못 붙이지 않기 위한 장치야.
package main

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"shearwater/internal/apis"
)

type eventReport struct {
	Revision string       `json:"resourceVersion"`
	Items    []apis.Event `json:"items"`
	Omitted  int          `json:"omitted"`
}

func readDiagnosticEvents(ctx context.Context, c *agentClient, o diagnosticOptions, uid string) (eventReport, error) {
	r := resourceRef{resource: apis.Lookup("", "v1", "events")}
	path := r.path(o.namespace)
	if uid != "" {
		path += "?" + url.Values{"fieldSelector": {"involvedObject.uid=" + uid}}.Encode()
	}
	var list inspectList[apis.Event]
	if err := diagnosticJSON(ctx, c, path, &list); err != nil {
		return eventReport{}, err
	}
	rev, err := strconv.ParseUint(list.Metadata.Revision, 10, 63)
	if err != nil {
		return eventReport{}, opsError("INTERNAL", "safe", "invalid Event list revision", "check the API server")
	}
	var since uint64
	if o.since != "" {
		since, _ = strconv.ParseUint(o.since, 10, 63)
		if since > rev {
			return eventReport{}, opsError("INVALID", "never", "--since is newer than the server revision", "use a revision observed on this server")
		}
	}
	result := eventReport{Revision: list.Metadata.Revision, Items: []apis.Event{}}
	for _, event := range list.Items {
		version, e := strconv.ParseUint(event.Metadata.ResourceVersion, 10, 63)
		if e != nil {
			return eventReport{}, opsError("INTERNAL", "safe", "invalid Event resourceVersion", "check the API server")
		}
		if uid != "" && event.InvolvedObject.UID != uid {
			continue
		}
		if version > since {
			result.Items = append(result.Items, event)
		}
	}
	// An Event is an aggregated record, not an immutable log entry: repetitions
	// increment count and lastTimestamp. Show most recently updated records first.
	slices.SortFunc(result.Items, func(a, b apis.Event) int {
		x, _ := strconv.ParseUint(a.Metadata.ResourceVersion, 10, 63)
		y, _ := strconv.ParseUint(b.Metadata.ResourceVersion, 10, 63)
		if x > y {
			return -1
		}
		if x < y {
			return 1
		}
		return strings.Compare(a.Metadata.Name, b.Metadata.Name)
	})
	if len(result.Items) > o.limit {
		result.Omitted = len(result.Items) - o.limit
		result.Items = result.Items[:o.limit]
	}
	return result, nil
}

func renderDiagnosticEvents(events eventReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "events rev=%s (most recently updated first)\n", events.Revision)
	if len(events.Items) == 0 {
		b.WriteString("No matching Event records.\n")
	}
	for _, event := range events.Items {
		when := "<unknown>"
		if event.LastTimestamp != nil {
			when = event.LastTimestamp.UTC().Format("2006-01-02T15:04:05Z")
		}
		fmt.Fprintf(&b, "%s %s %s/%s %s %s count=%d rv=%s: %s\n", when, event.Metadata.Namespace, event.InvolvedObject.Kind, event.InvolvedObject.Name, safeDiagnosticText(event.EventType), safeDiagnosticText(event.Reason), event.Count, event.Metadata.ResourceVersion, safeDiagnosticText(event.Message))
	}
	if events.Omitted > 0 {
		fmt.Fprintf(&b, "%d older matching record(s) omitted; increase --limit before advancing a --since cursor.\n", events.Omitted)
	}
	return b.String()
}

func diagnosticEvents(ctx context.Context, c *agentClient, o diagnosticOptions) ([]byte, error) {
	var object apis.Object
	var err error
	uid := ""
	if o.forObject != "" {
		object, err = diagnosticObject(ctx, c, o.ref, o.namespace)
		if err != nil {
			return nil, err
		}
		uid = object.Meta().UID
	}
	report, err := readDiagnosticEvents(ctx, c, o, uid)
	if err != nil {
		return nil, err
	}
	if object != nil {
		current, e := diagnosticObject(ctx, c, o.ref, o.namespace)
		if e != nil {
			return nil, e
		}
		if current.Meta().UID != uid {
			return nil, diagnosticChanged()
		}
	}
	if o.output == "json" {
		return prettyJSON(report)
	}
	return []byte(renderDiagnosticEvents(report)), nil
}

func diagnosticChanged() error {
	return opsError("CONFLICT", "safe", "the object or container attempt changed during the read", "repeat the command to inspect the current UID and attempt")
}

func chooseContainer(p *apis.Pod, name string) (string, error) {
	if name == "" && len(p.Spec.Containers) == 1 {
		return p.Spec.Containers[0].Name, nil
	}
	var names []string
	for _, container := range p.Spec.Containers {
		names = append(names, container.Name)
		if name == container.Name {
			return name, nil
		}
	}
	return "", opsError("INVALID", "never", fmt.Sprintf("choose one container from %v (got %q)", names, name), "pass -c NAME")
}

func containerStatus(p *apis.Pod, name string) *apis.ContainerStatus {
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == name {
			return &p.Status.ContainerStatuses[i]
		}
	}
	return nil
}

func sameLogAttempt(before, after *apis.Pod, name string) bool {
	if before.Metadata.UID != after.Metadata.UID {
		return false
	}
	a, b := containerStatus(before, name), containerStatus(after, name)
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.ContainerID == b.ContainerID && a.RestartCount == b.RestartCount
}

func readDiagnosticLog(ctx context.Context, c *agentClient, o diagnosticOptions, container string, previous bool) ([]byte, error) {
	q := url.Values{"container": {container}, "tailLines": {strconv.Itoa(o.tail)}, "limitBytes": {strconv.Itoa(o.bytes)}}
	if previous {
		q.Set("previous", "true")
	}
	body, err := diagnosticRead(ctx, c, o.ref.path(o.namespace)+"/log?"+q.Encode())
	if err == nil && len(body) > o.bytes {
		return nil, opsError("INTERNAL", "safe", "server did not honor the log byte limit", "check the server version")
	}
	return body, err
}

func diagnosticLogs(ctx context.Context, c *agentClient, o diagnosticOptions) ([]byte, error) {
	obj, err := diagnosticObject(ctx, c, o.ref, o.namespace)
	if err != nil {
		return nil, err
	}
	p := obj.(*apis.Pod)
	container, err := chooseContainer(p, o.container)
	if err != nil {
		return nil, err
	}
	body, err := readDiagnosticLog(ctx, c, o, container, o.previous)
	if err != nil {
		return nil, err
	}
	current, err := diagnosticObject(ctx, c, o.ref, o.namespace)
	if err != nil {
		return nil, err
	}
	if !sameLogAttempt(p, current.(*apis.Pod), container) {
		return nil, diagnosticChanged()
	}
	return body, nil
}
