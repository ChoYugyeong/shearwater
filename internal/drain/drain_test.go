package drain

import (
	"testing"
	"time"
)

func TestWithdrawalAcknowledgementAndBound(t *testing.T) {
	s := &State{}
	now := time.Now()
	changed := s.Changed()
	if s.Ready("pod", "intent", now.Add(time.Hour)) {
		t.Fatal("intent alone is not acknowledgement")
	}
	s.Observe(now, map[string]Observation{"pod": {Token: "intent", Active: 2}})
	select {
	case <-changed:
	default:
		t.Fatal("controller not woken")
	}
	if s.Ready("pod", "intent", now.Add(Timeout-time.Nanosecond)) {
		t.Fatal("terminated while connections active")
	}
	if !s.Ready("pod", "intent", now.Add(Timeout)) {
		t.Fatal("unbounded drain")
	}
	if s.Ready("pod", "new-intent", now.Add(time.Hour)) {
		t.Fatal("stale acknowledgement accepted")
	}
	s.Observe(now.Add(time.Second), map[string]Observation{"pod": {Token: "intent", Active: 0}})
	if !s.Ready("pod", "intent", now.Add(time.Second)) {
		t.Fatal("zero connections still waiting")
	}
	s.Observe(now, nil) // error / cancellation invalidates the observation
	if s.Ready("pod", "intent", now.Add(time.Hour)) {
		t.Fatal("failed observation treated as zero")
	}
	s.Observe(now.Add(time.Hour), map[string]Observation{"pod": {Token: "intent", Active: 1}})
	if s.Ready("pod", "intent", now.Add(time.Hour)) {
		t.Fatal("restart consumed the drain budget")
	}
}
