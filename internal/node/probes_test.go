package node

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"shearwater/internal/apis"
)

func TestRunningEventTriggersFirstReadinessProbe(t *testing.T) {
	e := newEnv(t)
	e.agent.Now, e.fake.Now = time.Now, time.Now
	p := e.createPod("starting-web", func(p *apis.Pod) {
		p.Spec.Containers[0].ReadinessProbe = json.RawMessage(`{"httpGet":{"path":"/health","port":8080},"periodSeconds":60}`)
	})
	probing := make(chan string)
	probeResult := make(chan error, 1)
	e.agent.ProbeRun = func(ctx context.Context, _ *apis.Probe, _ *apis.Container, _, id string) error {
		select {
		case probing <- id:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case err := <-probeResult:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.agent.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })

	// No writes from the test. The Running event triggers the first sample,
	// and a real failure must generate another before the 60-second period.
	for _, result := range []error{errors.New("listener starting"), nil} {
		select {
		case attempt := <-probing:
			cs := status(t, e.get(p))
			if cs.State.Running == nil || cs.ContainerID != containerID(attempt) || cs.Ready {
				t.Fatalf("readiness probe for %s saw stored status %+v", attempt, cs)
			}
			probeResult <- result
		case <-ctx.Done():
			t.Fatal("timed out waiting for initial readiness probes")
		}
	}
	for {
		changed := e.st.Changed()
		if status(t, e.get(p)).Ready {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("the successful probe did not publish Ready")
		}
	}
}

func TestInitialReadinessRetriesReturnToConfiguredPeriod(t *testing.T) {
	e := newEnv(t)
	p := e.createPod("unready", func(p *apis.Pod) {
		p.Spec.Containers[0].ReadinessProbe = json.RawMessage(`{"httpGet":{"path":"/health","port":8080},"periodSeconds":2,"failureThreshold":1}`)
	})
	calls := 0
	e.agent.ProbeRun = func(context.Context, *apis.Probe, *apis.Container, string, string) error {
		calls++
		return errors.New("unready")
	}
	e.sync(e.agent, p)
	for i, wait := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 2 * time.Second, 2 * time.Second} {
		if retry := e.sync(e.agent, p); calls != i+1 || retry != wait || status(t, e.get(p)).Ready {
			t.Fatalf("failed probe %d: calls %d, retry in %v, want %v", i+1, calls, retry, wait)
		}
		e.sync(e.agent, p)
		if calls != i+1 || len(e.fake.Signals) != 0 {
			t.Fatal("store wake repeated a sample or signalled the unready container")
		}
		e.clk.advance(wait)
	}
}

func TestProbeCountsSaturateAtThreshold(t *testing.T) {
	for _, tc := range []struct{ count, threshold, want int32 }{
		{0, 3, 1}, {2, 3, 3}, {3, 3, 3}, {math.MaxInt32, 3, 3},
		{math.MaxInt32 - 1, math.MaxInt32, math.MaxInt32},
		{math.MaxInt32, math.MaxInt32, math.MaxInt32},
	} {
		if got := nextProbeCount(tc.count, tc.threshold); got != tc.want {
			t.Errorf("next count from %d with threshold %d: got %d, want %d", tc.count, tc.threshold, got, tc.want)
		}
	}
}

func TestRunningEventHonorsReadinessInitialDelay(t *testing.T) {
	e := newEnv(t)
	p := e.createPod("delayed", func(p *apis.Pod) {
		p.Spec.Containers[0].ReadinessProbe = json.RawMessage(`{"tcpSocket":{"port":80},"initialDelaySeconds":3,"periodSeconds":2}`)
	})
	calls := 0
	e.agent.ProbeRun = func(context.Context, *apis.Probe, *apis.Container, string, string) error {
		calls++
		return nil
	}
	if retry := e.sync(e.agent, p); retry != 3*time.Second {
		t.Fatalf("initial delay: retry in %v", retry)
	}
	e.clk.advance(2 * time.Second)
	if retry := e.sync(e.agent, p); retry != time.Second || calls != 0 || status(t, e.get(p)).Ready {
		t.Fatalf("early Running event: retry in %v, probe calls %d", retry, calls)
	}
	e.clk.advance(time.Second)
	if retry := e.sync(e.agent, p); retry != 2*time.Second || calls != 1 || !status(t, e.get(p)).Ready {
		t.Fatalf("first due probe: retry in %v, probe calls %d", retry, calls)
	}
}

func TestSuccessfulStartupProbeTriggersReadinessAtStart(t *testing.T) {
	e := newEnv(t)
	p := e.createPod("startup-ready", func(p *apis.Pod) {
		p.Spec.Containers[0].StartupProbe = json.RawMessage(`{"exec":{"command":["true"]}}`)
		p.Spec.Containers[0].ReadinessProbe = json.RawMessage(`{"tcpSocket":{"port":80}}`)
	})
	calls := 0
	e.agent.ProbeRun = func(context.Context, *apis.Probe, *apis.Container, string, string) error {
		calls++
		return nil
	}
	e.sync(e.agent, p)
	if calls != 2 || !status(t, e.get(p)).Ready {
		t.Fatalf("successful startup should enable readiness in the same pass: calls %d", calls)
	}
}

func TestReadinessAfterContainerStartAndAgentRestart(t *testing.T) {
	e := newEnv(t)
	p := e.createPod("web", func(p *apis.Pod) {
		p.Spec.Containers[0].ReadinessProbe = json.RawMessage(`{"httpGet":{"path":"/health","port":8080},"periodSeconds":2,"failureThreshold":3}`)
	})
	listening := false
	calls := 0
	e.agent.ProbeRun = func(context.Context, *apis.Probe, *apis.Container, string, string) error {
		calls++
		if !listening {
			return errors.New("not listening yet")
		}
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			e.fake.Exit(id(p, attempt-1), 1)
		}
		listening = false
		e.sync(e.agent, p)
		cs := status(t, e.get(p))
		if cs.State.Running == nil || cs.ContainerID != containerID(id(p, attempt)) || cs.Ready || calls != attempt {
			t.Fatalf("start of attempt %d: status %+v, probe calls %d", attempt, cs, calls)
		}
		// The app opens its listener between task start and the next Pod
		// observation. It can pass then, well inside the two-second period.
		e.clk.advance(20 * time.Millisecond)
		listening = true
		retry := e.sync(e.agent, p)
		if !status(t, e.get(p)).Ready || calls != attempt+1 || retry != 2*time.Second {
			t.Fatalf("first sample for attempt %d: calls %d, next probe in %v", attempt, calls, retry)
		}
	}

	// A new observer already has a published Running ID. It must sample
	// it immediately and cannot reuse the last observer's successful result.
	next := e.newAgent()
	next.DataDir, next.ProbeRun = e.agent.DataDir, e.agent.ProbeRun
	listening = false
	if retry := e.sync(next, p); status(t, e.get(p)).Ready || calls != 3 || retry != 2*time.Second {
		t.Fatalf("adopted readiness: calls %d, next probe in %v, status %+v", calls, retry, status(t, e.get(p)))
	}
}

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
	e.sync(e.agent, p) // first sample after Running was published
	e.sync(e.agent, p)
	if calls != 1 {
		t.Fatal("status update bypassed probe period")
	}
	ok = true
	e.clk.advance(100 * time.Millisecond)
	if retry := e.sync(e.agent, p); calls != 2 || retry != 2*time.Second || status(t, e.get(p)).Ready {
		t.Fatalf("first early success: calls %d, next probe in %v, status %+v", calls, retry, status(t, e.get(p)))
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
	ok = true
	e.clk.advance(100 * time.Millisecond)
	e.sync(e.agent, p)
	if calls != 5 || status(t, e.get(p)).Ready {
		t.Fatal("startup retries resumed after an established container lost readiness")
	}
	e.clk.advance(1900 * time.Millisecond)
	e.sync(e.agent, p)
	if status(t, e.get(p)).Ready {
		t.Fatal("recovery bypassed the success threshold")
	}
	e.clk.advance(2 * time.Second)
	e.sync(e.agent, p)
	if !status(t, e.get(p)).Ready {
		t.Fatal("did not recover after two full-period successes")
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
