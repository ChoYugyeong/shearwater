package node

import (
	"context"
	"errors"
	"testing"
)

// A failed kernel cleanup must reserve the old IP even after the container
// has stopped. Otherwise another Pod could inherit a live connection's IP.
func TestRetirementFailureKeepsPodIPUntilRetrySucceeds(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("retiring", nil)
	e.sync(e.agent, pod)
	before, err := e.st.IPs("pod")
	if err != nil || before[pod.Metadata.UID] == "" {
		t.Fatalf("missing initial reservation: %v %v", before, err)
	}
	failure := errors.New("conntrack cleanup unavailable")
	calls := 0
	e.agent.RetirePod = func(_ context.Context, uid string) error {
		calls++
		ips, err := e.st.IPs("pod")
		if err != nil || uid != pod.Metadata.UID || ips[uid] != before[uid] {
			t.Fatalf("IP released before kernel retirement: %v %v", ips, err)
		}
		return failure
	}
	e.markDeleted(pod, 0)
	e.sync(e.agent, pod) // request KILL; the fake runtime records the exit
	e.sync(e.agent, pod) // clean up the stopped task and finish object deletion
	if calls != 0 || e.get(pod) != nil {
		t.Fatal("retirement must wait until the Pod object is gone")
	}
	retry, err := e.agent.SyncPod(context.Background(), pod.Metadata.UID)
	if !errors.Is(err, failure) || retry <= 0 || calls != 1 {
		t.Fatalf("failed retirement did not retry: %v %v calls=%d", retry, err, calls)
	}
	retained, err := e.st.IPs("pod")
	if err != nil || retained[pod.Metadata.UID] != before[pod.Metadata.UID] {
		t.Fatalf("failed retirement lost IP: %v %v", retained, err)
	}
	failure = nil
	e.sync(e.agent, pod)
	after, err := e.st.IPs("pod")
	if err != nil || after[pod.Metadata.UID] != "" || calls != 2 {
		t.Fatalf("successful retry did not release IP: %v %v calls=%d", after, err, calls)
	}
}
