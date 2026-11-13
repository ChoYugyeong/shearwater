// why는 "확정된 근본 원인"을 추측하는 명령이 아니야. 서버가 관찰한 상태,
// 관련 이벤트와 제한된 로그를 모으고, 다음에 확인할 내용을 구분해 보여 줘.
// 근거가 없으면 unavailable로 남기고 자동 수정·재시작은 하지 않아.
package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"shearwater/internal/agent"
	"shearwater/internal/apis"
)

type logExcerpt struct {
	Container      string `json:"container"`
	Previous       bool   `json:"previous"`
	Text           string `json:"text"`
	Unavailable    string `json:"unavailable,omitempty"`
	MayBeTruncated bool   `json:"mayBeTruncated"`
}

type podExplanation struct {
	Namespace  string                 `json:"namespace"`
	Name       string                 `json:"name"`
	UID        string                 `json:"uid"`
	Revision   string                 `json:"resourceVersion"`
	Summary    string                 `json:"summary"`
	Phase      string                 `json:"phase"`
	Containers []apis.ContainerStatus `json:"containers"`
	Findings   []agent.Finding        `json:"findings"`
	Events     eventReport            `json:"events"`
	Logs       []logExcerpt           `json:"logs"`
	LimitNote  string                 `json:"limitNote"`
}

func diagnosticWhy(ctx context.Context, c *agentClient, o diagnosticOptions) ([]byte, error) {
	// A Pod may restart while we read it. Retry a small number of times instead
	// of joining the old state with the next attempt's logs. Events and logs are
	// still observations, not a transactionally frozen history; say so in output.
	for range 3 {
		obj, err := diagnosticObject(ctx, c, o.ref, o.namespace)
		if err != nil {
			return nil, err
		}
		p := obj.(*apis.Pod)
		var containers []string
		if o.container != "" {
			name, e := chooseContainer(p, o.container)
			if e != nil {
				return nil, e
			}
			containers = []string{name}
		} else {
			if len(p.Spec.Containers) > 8 {
				return nil, opsError("INVALID", "never", "why limits automatic log collection to eight containers", "pass -c NAME to inspect one container")
			}
			for _, container := range p.Spec.Containers {
				containers = append(containers, container.Name)
			}
		}
		events, err := readDiagnosticEvents(ctx, c, o, p.Metadata.UID)
		if err != nil {
			return nil, err
		}
		report := podExplanation{Namespace: p.Metadata.Namespace, Name: p.Metadata.Name, UID: p.Metadata.UID, Revision: p.Metadata.ResourceVersion,
			Summary: agent.CompactObject(p), Phase: p.Status.Phase, Containers: p.Status.ContainerStatuses, Findings: agent.PodFindings(p), Events: events, Logs: []logExcerpt{},
			LimitNote: "Read-only observations, not a frozen history. Logs are bounded per excerpt. Configured readiness/startup probes affect Ready; no probe means no application response check."}
		for _, name := range containers {
			cs := containerStatus(p, name)
			if cs == nil || cs.ContainerID == "" {
				report.Logs = append(report.Logs, logExcerpt{Container: name, Unavailable: "container has not started; no container log is available"})
				continue
			}
			attempts := []bool{false}
			if cs.RestartCount > 0 {
				attempts = append(attempts, true)
			}
			for _, previous := range attempts {
				excerpt := logExcerpt{Container: name, Previous: previous}
				body, e := readDiagnosticLog(ctx, c, o, name, previous)
				if e != nil {
					var op *operationError
					// A previous log may have been removed, or the container may
					// never have started. Keep that evidence gap visible. Auth,
					// transport and server failures must fail the whole command.
					if errors.As(e, &op) && (op.code == "INVALID" || op.code == "NOT_FOUND" || op.code == "UNSUPPORTED") {
						excerpt.Unavailable = e.Error()
					} else {
						return nil, e
					}
				} else {
					excerpt.Text = string(body)
					excerpt.MayBeTruncated = len(body) == o.bytes
				}
				report.Logs = append(report.Logs, excerpt)
			}
		}
		current, err := diagnosticObject(ctx, c, o.ref, o.namespace)
		if err != nil {
			return nil, err
		}
		if current.Meta().UID != p.Metadata.UID || current.Meta().ResourceVersion != p.Metadata.ResourceVersion {
			continue
		}
		if o.output == "json" {
			return prettyJSON(report)
		}
		return []byte(renderPodExplanation(report)), nil
	}
	return nil, diagnosticChanged()
}

func renderPodExplanation(report podExplanation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pod/%s namespace=%s uid=%s rv=%s\nstate: %s\n", report.Name, report.Namespace, report.UID, report.Revision, report.Summary)
	for _, finding := range report.Findings {
		fmt.Fprintf(&b, "finding %s: %s\n  next: %s\n", safeDiagnosticText(finding.Code), safeDiagnosticText(finding.Message), finding.Next)
	}
	for _, cs := range report.Containers {
		fmt.Fprintf(&b, "container %s image=%s ready=%t restarts=%d\n", cs.Name, safeDiagnosticText(cs.Image), cs.Ready, cs.RestartCount)
		if ended := cs.State.Terminated; ended != nil {
			fmt.Fprintf(&b, "  current exit=%d reason=%s message=%s\n", ended.ExitCode, safeDiagnosticText(ended.Reason), safeDiagnosticText(ended.Message))
		}
		if ended := cs.LastState.Terminated; ended != nil {
			fmt.Fprintf(&b, "  previous exit=%d reason=%s message=%s\n", ended.ExitCode, safeDiagnosticText(ended.Reason), safeDiagnosticText(ended.Message))
		}
	}
	b.WriteString(renderDiagnosticEvents(report.Events))
	for _, log := range report.Logs {
		attempt := "current"
		if log.Previous {
			attempt = "previous"
		}
		fmt.Fprintf(&b, "logs container=%s attempt=%s\n", log.Container, attempt)
		if log.Unavailable != "" {
			fmt.Fprintf(&b, "  unavailable: %s\n", safeDiagnosticText(log.Unavailable))
			continue
		}
		if log.Text == "" {
			b.WriteString("  (empty log)\n")
		}
		for _, line := range strings.Split(strings.TrimSuffix(log.Text, "\n"), "\n") {
			if log.Text != "" {
				fmt.Fprintf(&b, "  | %s\n", strconv.Quote(line))
			}
		}
		if log.MayBeTruncated {
			b.WriteString("  byte limit reached; excerpt may be truncated\n")
		}
	}
	fmt.Fprintf(&b, "note: %s\n", report.LimitNote)
	return b.String()
}
