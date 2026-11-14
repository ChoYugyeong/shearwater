// Package drain coordinates planned Pod retirement with the Service data path.
// The annotation is the durable intent; acknowledgements are deliberately local
// to one server process. After a crash we must verify the kernel again, rather
// than trusting an old "zero connections" observation.
package drain

import (
	"sync"
	"time"
)

const Annotation = "shearwater.io/connection-drain"
const Timeout = 30 * time.Second

type Observation struct {
	Token  string
	Active int
}

type entry struct {
	Observation
	since time.Time
}

type State struct {
	mu      sync.Mutex
	entries map[string]entry
	changed chan struct{}
}

func (s *State) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	return s.changed
}

func (s *State) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries) > 0
}

// Observe is called only AFTER new connections have been withdrawn in the
// kernel and a complete conntrack dump has succeeded. An error is never zero.
func (s *State) Observe(now time.Time, observations map[string]Observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]entry, len(observations))
	changed := len(observations) != len(s.entries)
	for uid, o := range observations {
		e, ok := s.entries[uid]
		changed = changed || !ok || e.Observation != o
		if !ok || e.Token != o.Token {
			e.since = now
		}
		e.Observation = o
		next[uid] = e
	}
	s.entries = next
	if changed && s.changed != nil {
		close(s.changed)
		s.changed = make(chan struct{})
	}
}

// Ready permits normal termination after the last connection closes, or after
// a bounded drain. The bound starts at successful withdrawal, not at a possibly
// old annotation timestamp. Thus downtime cannot silently consume the grace.
func (s *State) Ready(uid, token string, now time.Time) bool {
	ready, _ := s.Decision(uid, token, now)
	return ready
}

// Decision distinguishes an empty drain from the bounded, potentially
// disruptive timeout so the controller can explain it in a Pod event.
func (s *State) Decision(uid, token string, now time.Time) (ready, timedOut bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[uid]
	if !ok || e.Token != token {
		return false, false
	}
	timedOut = e.Active > 0 && !now.Before(e.since.Add(Timeout))
	return e.Active == 0 || timedOut, timedOut
}
