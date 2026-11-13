package node

import (
	"context"
	"encoding/json"
	"errors"
	"shearwater/internal/apis"
	"testing"
	"time"
)

func TestReadinessThresholdAndRecovery(t *testing.T) {
	e := newEnv(t)
	ok := false
	calls := 0
	e.agent.ProbeRun = func(context.Context, *apis.Probe, *apis.Container, string, string) error {
		calls++
		if !ok {
			return errors.New("unavailable")
		}
		return nil
	}
	p := e.createPod("probe", func(p *apis.Pod) {
		p.Spec.Containers[0].ReadinessProbe = json.RawMessage(`{"httpGet":{"path":"/","port":80},"periodSeconds":2,"failureThreshold":2,"successThreshold":2}`)
	})
	e.sync(e.agent, p)
	if e.get(p).Status.ContainerStatuses[0].Ready {
		t.Fatal("ready before success")
	}
	e.sync(e.agent, p)
	if calls != 1 {
		t.Fatal("status update bypassed probe period")
	}
	ok = true
	e.clk.advance(2 * time.Second)
	e.sync(e.agent, p)
	if e.get(p).Status.ContainerStatuses[0].Ready {
		t.Fatal("success threshold ignored")
	}
	e.clk.advance(2 * time.Second)
	e.sync(e.agent, p)
	if !e.get(p).Status.ContainerStatuses[0].Ready {
		t.Fatal("did not become ready")
	}
	ok = false
	e.clk.advance(2 * time.Second)
	e.sync(e.agent, p)
	if !e.get(p).Status.ContainerStatuses[0].Ready {
		t.Fatal("failure threshold ignored")
	}
	e.clk.advance(2 * time.Second)
	e.sync(e.agent, p)
	if e.get(p).Status.ContainerStatuses[0].Ready {
		t.Fatal("failed readiness still ready")
	}
	if len(e.fake.Signals) != 0 {
		t.Fatal("readiness killed the app")
	}
}
func TestStartupGatesLivenessAndPersists(t *testing.T) {
	e := newEnv(t)
	startupOK := false
	liveCalls := 0
	run := func(_ context.Context, q *apis.Probe, _ *apis.Container, _, _ string) error {
		if q.TCPSocket != nil {
			if !startupOK {
				return errors.New("starting")
			}
			return nil
		}
		liveCalls++
		return nil
	}
	e.agent.ProbeRun = run
	p := e.createPod("starting", func(p *apis.Pod) {
		p.Spec.Containers[0].StartupProbe = json.RawMessage(`{"tcpSocket":{"port":80},"periodSeconds":1,"failureThreshold":10}`)
		p.Spec.Containers[0].LivenessProbe = json.RawMessage(`{"exec":{"command":["true"]},"periodSeconds":1}`)
	})
	e.sync(e.agent, p)
	if liveCalls != 0 || *e.get(p).Status.ContainerStatuses[0].Started {
		t.Fatal("startup did not gate liveness")
	}
	startupOK = true
	e.clk.advance(time.Second)
	e.sync(e.agent, p)
	if liveCalls != 1 {
		t.Fatal("liveness did not start")
	}
	next := e.newAgent()
	next.DataDir = e.agent.DataDir
	next.ProbeRun = run
	startupOK = false
	e.clk.advance(time.Second)
	e.sync(next, p)
	if !*e.get(p).Status.ContainerStatuses[0].Started || liveCalls != 2 {
		t.Fatal("startup completion was lost on adoption")
	}
}
func TestLivenessGraceRestartAndNever(t *testing.T) {
	for _, policy := range []string{"Always", "OnFailure", "Never"} {
		t.Run(policy, func(t *testing.T) {
			e := newEnv(t)
			e.fake.IgnoreTERM = true
			e.agent.ProbeRun = func(context.Context, *apis.Probe, *apis.Container, string, string) error { return errors.New("hung") }
			p := e.createPod("hung", func(p *apis.Pod) {
				p.Spec.RestartPolicy = policy
				p.Spec.Containers[0].LivenessProbe = json.RawMessage(`{"exec":{"command":["false"]},"periodSeconds":1,"failureThreshold":1,"terminationGracePeriodSeconds":2}`)
			})
			e.sync(e.agent, p)
			before := e.get(p).Status.ContainerStatuses[0]
			if before.Ready || len(e.fake.Signals) != 1 {
				t.Fatal("did not request graceful stop")
			}
			// Stop intent and deadline survive a server restart during the grace period.
			next := e.newAgent()
			next.DataDir = e.agent.DataDir
			next.ProbeRun = func(context.Context, *apis.Probe, *apis.Container, string, string) error { return nil }
			e.clk.advance(2 * time.Second)
			e.sync(next, p)
			e.sync(next, p)
			after := e.get(p).Status.ContainerStatuses[0]
			if policy == "Never" {
				if after.State.Terminated == nil {
					t.Fatal("Never restarted")
				}
			} else if after.RestartCount != 1 || after.ContainerID == before.ContainerID {
				t.Fatal("failed probe did not restart")
			}
		})
	}
}
