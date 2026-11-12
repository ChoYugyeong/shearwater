package api

// Watch tests, in two layers. The selector table in
// testdata/watch-transitions.json is applied twice: once straight to
// filter.event (the before/after decision) and once through the store's change
// log, where the same events must come out of real writes. The second layer
// drives the HTTP stream the way kubectl and its clients do.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

// ---- the fixed transition table (testdata/watch-transitions.json) ----

type watchState struct {
	Labels   map[string]string `json:"labels"`
	NodeName string            `json:"nodeName"`
	Phase    string            `json:"phase"`
}

type watchStep struct {
	Do     string  `json:"do"`
	State  string  `json:"state"`
	Name   string  `json:"name"`
	Event  *string `json:"event"` // null means "nothing is sent"
	Object string  `json:"object"`
}

type watchTransition struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Event  *string `json:"event"`
	Object string  `json:"object"`
}

type watchSelector struct {
	Name  string      `json:"name"`
	Query string      `json:"query"`
	In    watchState  `json:"in"`
	Out   watchState  `json:"out"`
	Steps []watchStep `json:"steps"`
}

type watchSequence struct {
	Name  string      `json:"name"`
	Steps []watchStep `json:"steps"`
}

type watchFixture struct {
	Transitions  []watchTransition `json:"transitions"`
	Selectors    []watchSelector   `json:"selectors"`
	Sequences    []watchSequence   `json:"sequences"`
	MetadataName struct {
		Query string      `json:"query"`
		Steps []watchStep `json:"steps"`
	} `json:"metadataName"`
}

func loadWatchFixture(t *testing.T) watchFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/watch-transitions.json")
	if err != nil {
		t.Fatalf("reading the fixed transition table: %v", err)
	}
	var f watchFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("testdata/watch-transitions.json: %v", err)
	}
	if len(f.Transitions) == 0 || len(f.Selectors) == 0 || len(f.Sequences) == 0 {
		t.Fatal("testdata/watch-transitions.json has an empty section")
	}
	return f
}

func (s watchSelector) state(name string) watchState {
	switch name {
	case "in":
		return s.In
	case "out":
		return s.Out
	}
	return watchState{}
}

func podsRes() *apis.Resource { return apis.Lookup("", "v1", "pods") }

// pod builds the object a state describes.
func (s watchState) pod(name string) *apis.Pod {
	p := &apis.Pod{}
	p.Metadata.Name = name
	p.Metadata.Namespace = "default"
	if len(s.Labels) > 0 {
		p.Metadata.Labels = make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			p.Metadata.Labels[k] = v
		}
	}
	p.Spec.NodeName = s.NodeName
	p.Spec.Containers = []apis.Container{{Name: "c", Image: "nginx:1.27"}}
	p.Status.Phase = s.Phase
	return p
}

func podBytes(t *testing.T, s watchState, name string, rev int64) []byte {
	t.Helper()
	p := s.pod(name)
	p.Metadata.ResourceVersion = strconv.FormatInt(rev, 10)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("encoding a pod: %v", err)
	}
	return b
}

// checkPodState says the event object is the pod this state describes, with
// rev as its resourceVersion.
func checkPodState(t *testing.T, data []byte, want watchState, rev int64) {
	t.Helper()
	var p apis.Pod
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("the event object is not a pod: %v (%s)", err, data)
	}
	if len(p.Metadata.Labels) != len(want.Labels) {
		t.Fatalf("labels are %v, want %v", p.Metadata.Labels, want.Labels)
	}
	for k, v := range want.Labels {
		if p.Metadata.Labels[k] != v {
			t.Fatalf("labels are %v, want %v", p.Metadata.Labels, want.Labels)
		}
	}
	if p.Spec.NodeName != want.NodeName {
		t.Fatalf("nodeName is %q, want %q", p.Spec.NodeName, want.NodeName)
	}
	if p.Status.Phase != want.Phase {
		t.Fatalf("phase is %q, want %q", p.Status.Phase, want.Phase)
	}
	if p.Metadata.ResourceVersion != strconv.FormatInt(rev, 10) {
		t.Fatalf("resourceVersion is %q, want %d", p.Metadata.ResourceVersion, rev)
	}
}

// TestWatchTransitionTable is the before/after matrix, one filter per selector
// in the fixture, with no store involved.
func TestWatchTransitionTable(t *testing.T) {
	f := loadWatchFixture(t)
	res := podsRes()
	for _, sel := range f.Selectors {
		t.Run(sel.Name, func(t *testing.T) {
			q, err := url.ParseQuery(sel.Query)
			if err != nil {
				t.Fatalf("the fixture's query %q: %v", sel.Query, err)
			}
			fil, err := parseFilter(q, res)
			if err != nil {
				t.Fatalf("parseFilter(%q): %v", sel.Query, err)
			}
			for _, tr := range f.Transitions {
				const rev = 41
				from, to := sel.state(tr.From), sel.state(tr.To)
				c := store.Change{Rev: rev, Type: "MODIFIED"}
				if tr.From != "none" {
					c.Prev = podBytes(t, from, "p1", rev-1)
				}
				if tr.To == "none" {
					c.Type = "DELETED"
					c.Data = podBytes(t, from, "p1", rev)
				} else {
					c.Data = podBytes(t, to, "p1", rev)
				}
				typ, data, err := fil.event(c)
				if err != nil {
					t.Fatalf("%s -> %s: %v", tr.From, tr.To, err)
				}
				want := ""
				if tr.Event != nil {
					want = *tr.Event
				}
				if typ != want {
					t.Fatalf("%s -> %s: the event is %q, want %q", tr.From, tr.To, typ, want)
				}
				if typ == "" {
					continue
				}
				switch {
				case tr.Object == "after":
					checkPodState(t, data, to, rev)
				default: // "before", with this change's resourceVersion
					checkPodState(t, data, from, rev)
				}
			}
		})
	}
}

// watchStore writes pods straight into a store, so nodeName and phase can be
// set the way the node agent will set them.
type watchStore struct {
	t    *testing.T
	st   *store.Store
	res  *apis.Resource
	pods map[string]*apis.Pod
}

func newWatchStore(t *testing.T) *watchStore {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &watchStore{t: t, st: st, res: podsRes(), pods: map[string]*apis.Pod{}}
}

func (w *watchStore) write(what string, fn func(tx *store.Tx) error) {
	w.t.Helper()
	if err := w.st.Write(false, fn); err != nil {
		w.t.Fatalf("%s: %v", what, err)
	}
}

func (w *watchStore) create(s watchState, name string) {
	w.t.Helper()
	p := s.pod(name)
	stamp(w.res, p)
	p.Status.Phase = s.Phase // stamp's Prepare is what makes a new pod Pending
	w.write("create "+name, func(tx *store.Tx) error {
		_, err := tx.Create(put(w.res, p))
		return err
	})
	w.pods[name] = p
}

func (w *watchStore) update(s watchState, name string, seq int) {
	w.t.Helper()
	p, ok := w.pods[name]
	if !ok {
		w.t.Fatalf("update of %s, which was never created", name)
	}
	p.Metadata.Labels = nil
	if len(s.Labels) > 0 {
		p.Metadata.Labels = make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			p.Metadata.Labels[k] = v
		}
	}
	// A field no selector reads, so an update always changes something even when
	// the pod stays on the same side of the selector.
	p.Metadata.Annotations = map[string]string{"watch-transitions.test/step": strconv.Itoa(seq)}
	p.Spec.NodeName = s.NodeName
	p.Status.Phase = s.Phase
	w.write("update "+name, func(tx *store.Tx) error {
		_, _, err := tx.Update(put(w.res, p), 0)
		return err
	})
}

func (w *watchStore) delete(name string) {
	w.t.Helper()
	p, ok := w.pods[name]
	if !ok {
		w.t.Fatalf("delete of %s, which was never created", name)
	}
	w.write("delete "+name, func(tx *store.Tx) error {
		_, err := tx.Delete(keyOf(w.res, p.Metadata.Namespace, p.Metadata.Name), 0, encoder(p))
		return err
	})
	delete(w.pods, name)
}

// changeEvent is one event as a watcher with the filter would print it.
type changeEvent struct {
	typ  string
	data []byte
	rev  int64
}

// changes reads the log after cursor and turns every entry into events.
func (w *watchStore) changes(f filter, cursor int64) ([]changeEvent, int64) {
	w.t.Helper()
	cs, next, err := w.st.Changes("", "pods", "default", cursor, 200)
	if err != nil {
		w.t.Fatalf("reading the change log after %d: %v", cursor, err)
	}
	var out []changeEvent
	for _, c := range cs {
		typ, data, err := f.event(c)
		if err != nil {
			w.t.Fatalf("filtering change %d: %v", c.Rev, err)
		}
		if typ != "" {
			out = append(out, changeEvent{typ: typ, data: data, rev: c.Rev})
		}
	}
	return out, next
}

// TestWatchSelectorSequences runs the fixture's step lists through real writes
// and the real change log.
func TestWatchSelectorSequences(t *testing.T) {
	f := loadWatchFixture(t)
	for _, sel := range f.Selectors {
		for _, seq := range f.Sequences {
			t.Run(sel.Name+"/"+seq.Name, func(t *testing.T) {
				w := newWatchStore(t)
				q, err := url.ParseQuery(sel.Query)
				if err != nil {
					t.Fatalf("the fixture's query %q: %v", sel.Query, err)
				}
				fil, err := parseFilter(q, podsRes())
				if err != nil {
					t.Fatalf("parseFilter(%q): %v", sel.Query, err)
				}
				var cursor int64
				var cur watchState
				for i, step := range seq.Steps {
					before := cur
					switch step.Do {
					case "create":
						cur = sel.state(step.State)
						w.create(cur, "p1")
					case "update":
						cur = sel.state(step.State)
						w.update(cur, "p1", i)
					case "delete":
						w.delete("p1")
						cur = watchState{}
					default:
						t.Fatalf("the fixture has an unknown step %q", step.Do)
					}
					events, next := w.changes(fil, cursor)
					cursor = next
					var want string
					if step.Event != nil {
						want = *step.Event
					}
					if want == "" {
						if len(events) != 0 {
							t.Fatalf("step %d (%s): got %s of %s, want no event",
								i, step.Do, events[0].typ, step.State)
						}
						continue
					}
					if len(events) != 1 {
						t.Fatalf("step %d (%s): got %d events, want one %s", i, step.Do, len(events), want)
					}
					if events[0].typ != want {
						t.Fatalf("step %d (%s): the event is %s, want %s", i, step.Do, events[0].typ, want)
					}
					if step.Object == "after" {
						checkPodState(t, events[0].data, cur, events[0].rev)
					} else {
						checkPodState(t, events[0].data, before, events[0].rev)
					}
				}
			})
		}
	}
}

// TestWatchMetadataName: a name cannot change, so only none->in, in->in and
// in->none happen for the named pod, and every change to another pod is
// out->out.
func TestWatchMetadataName(t *testing.T) {
	f := loadWatchFixture(t)
	w := newWatchStore(t)
	q, err := url.ParseQuery(f.MetadataName.Query)
	if err != nil {
		t.Fatalf("the fixture's query %q: %v", f.MetadataName.Query, err)
	}
	fil, err := parseFilter(q, podsRes())
	if err != nil {
		t.Fatalf("parseFilter(%q): %v", f.MetadataName.Query, err)
	}
	state := watchState{Labels: map[string]string{"app": "web"}, Phase: "Running"}
	var cursor int64
	for i, step := range f.MetadataName.Steps {
		switch step.Do {
		case "create":
			w.create(state, step.Name)
		case "update":
			w.update(state, step.Name, i)
		case "delete":
			w.delete(step.Name)
		default:
			t.Fatalf("the fixture has an unknown step %q", step.Do)
		}
		events, next := w.changes(fil, cursor)
		cursor = next
		if step.Event == nil {
			if len(events) != 0 {
				t.Fatalf("step %d (%s %s): got %s, want no event", i, step.Do, step.Name, events[0].typ)
			}
			continue
		}
		if len(events) != 1 || events[0].typ != *step.Event {
			t.Fatalf("step %d (%s %s): got %v, want one %s", i, step.Do, step.Name, events, *step.Event)
		}
		checkPodState(t, events[0].data, state, events[0].rev)
	}
}

// ---- the HTTP stream ----

func streamServer(t *testing.T) (*client, string) {
	t.Helper()
	c := newClient(t)
	ts := httptest.NewServer(c.srv)
	t.Cleanup(ts.Close)
	return c, ts.URL
}

// newPrunedClient keeps enough of the change log that a watcher can fall behind:
// one entry, no time window, and a compaction on every write.
func newPrunedClient(t *testing.T) *client {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	st.KeepChanges, st.KeepFor, st.CompactEvery = 1, -time.Hour, 0
	srv := &Server{Store: st, Token: "secret"}
	if err := srv.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	return &client{t: t, srv: srv}
}

type stream struct {
	t     *testing.T
	resp  *http.Response
	lines chan []byte
	done  chan error
}

// openStream starts a watch and reads it line by line, so every read can be
// given its own deadline.
func openStream(t *testing.T, base, path, accept string) *stream {
	t.Helper()
	req, err := http.NewRequest("GET", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("%s answered HTTP %d, want 200: %s", path, resp.StatusCode, body)
	}
	s := &stream{t: t, resp: resp, lines: make(chan []byte, 64), done: make(chan error, 1)}
	go func() {
		defer close(s.lines)
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				s.lines <- append([]byte(nil), line...)
			}
			if err != nil {
				s.done <- err
				return
			}
		}
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return s
}

func (s *stream) next(timeout time.Duration) map[string]any {
	s.t.Helper()
	select {
	case line, ok := <-s.lines:
		if !ok {
			s.t.Fatal("the stream ended before an event arrived")
		}
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			s.t.Fatalf("not one JSON event per line: %q", line)
		}
		return ev
	case err := <-s.done:
		s.t.Fatalf("the stream ended: %v", err)
	case <-time.After(timeout):
		s.t.Fatalf("no event within %v", timeout)
	}
	return nil
}

// quiet says nothing arrives for d.
func (s *stream) quiet(d time.Duration) {
	s.t.Helper()
	select {
	case line, ok := <-s.lines:
		if ok {
			s.t.Fatalf("an event arrived, want none: %s", line)
		}
	case <-time.After(d):
	}
}

// end says the stream closes on its own within timeout.
func (s *stream) end(timeout time.Duration) {
	s.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case line, ok := <-s.lines:
			if !ok {
				return
			}
			s.t.Fatalf("the stream sent an event, want it to end: %s", line)
		case err := <-s.done:
			if err != nil && err != io.EOF {
				s.t.Fatalf("reading the stream: %v", err)
			}
		case <-deadline:
			s.t.Fatal("the stream did not end in time")
		}
	}
}

func evType(ev map[string]any) string {
	s, _ := ev["type"].(string)
	return s
}

func evObject(ev map[string]any) map[string]any {
	m, _ := ev["object"].(map[string]any)
	return m
}

func evName(ev map[string]any) string {
	md, _ := evObject(ev)["metadata"].(map[string]any)
	s, _ := md["name"].(string)
	return s
}

func otherPod(name string) string {
	return strings.ReplaceAll(podBody, `"p1"`, `"`+name+`"`)
}

func listRV(t *testing.T, c *client) string {
	t.Helper()
	r := c.want(c.do("GET", pods, ""), 200)
	rv, _ := r.at("metadata", "resourceVersion").(string)
	if rv == "" {
		t.Fatalf("the list has no resourceVersion: %s", r.raw)
	}
	return rv
}

// TestWatchStreamSendsCurrentState: without a resourceVersion the watcher first
// gets the objects that exist, then the changes after them.
func TestWatchStreamSendsCurrentState(t *testing.T) {
	c, base := streamServer(t)
	c.want(c.do("POST", pods, podBody), 201)
	s := openStream(t, base, pods+"?watch=true", "")
	if ev := s.next(3 * time.Second); evType(ev) != "ADDED" || evName(ev) != "p1" {
		t.Fatalf("the first event is %s of %q, want ADDED p1", evType(ev), evName(ev))
	}
	c.want(c.do("POST", pods, otherPod("p2")), 201)
	if ev := s.next(3 * time.Second); evType(ev) != "ADDED" || evName(ev) != "p2" {
		t.Fatalf("the event after the create is %s of %q, want ADDED p2", evType(ev), evName(ev))
	}
}

// TestWatchStreamFromRevision: resourceVersion=N starts after N, with no
// initial state.
func TestWatchStreamFromRevision(t *testing.T) {
	c, base := streamServer(t)
	c.want(c.do("POST", pods, podBody), 201)
	rv := listRV(t, c)
	s := openStream(t, base, pods+"?watch=true&resourceVersion="+rv, "")
	c.want(c.do("POST", pods, otherPod("p2")), 201)
	if ev := s.next(3 * time.Second); evType(ev) != "ADDED" || evName(ev) != "p2" {
		t.Fatalf("a watch from revision %s sent %s of %q, want only ADDED p2 after it", rv, evType(ev), evName(ev))
	}
	s.quiet(300 * time.Millisecond)
}

// TestWatchStreamSendInitialEvents: the initial objects, then a BOOKMARK that
// says the initial events are over.
func TestWatchStreamSendInitialEvents(t *testing.T) {
	c, base := streamServer(t)
	c.want(c.do("POST", pods, podBody), 201)
	s := openStream(t, base, pods+"?watch=true&sendInitialEvents=true&resourceVersionMatch=NotOlderThan", "")
	if ev := s.next(3 * time.Second); evType(ev) != "ADDED" || evName(ev) != "p1" {
		t.Fatalf("the first event is %s of %q, want ADDED p1", evType(ev), evName(ev))
	}
	ev := s.next(3 * time.Second)
	if evType(ev) != "BOOKMARK" {
		t.Fatalf("the second event is %s, want BOOKMARK", evType(ev))
	}
	ann, _ := evObject(ev)["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann["k8s.io/initial-events-end"] != "true" {
		t.Fatalf("the bookmark must carry k8s.io/initial-events-end: %v", evObject(ev))
	}
	c.want(c.do("POST", pods, otherPod("p2")), 201)
	if ev := s.next(3 * time.Second); evType(ev) != "ADDED" || evName(ev) != "p2" {
		t.Fatalf("after the bookmark: %s of %q, want ADDED p2", evType(ev), evName(ev))
	}
}

// TestWatchStreamCompactedRevision: starting inside the part of the log that has
// been compacted is a 410, not a silent gap.
func TestWatchStreamCompactedRevision(t *testing.T) {
	c := newPrunedClient(t)
	c.want(c.do("POST", pods, podBody), 201) // revision 3; Bootstrap took 1 and 2
	r := c.want(c.do("GET", pods+"?watch=true&resourceVersion=1", ""), 410)
	if r.at("reason") != "Expired" {
		t.Fatalf("want reason Expired: %s", r.raw)
	}
	if compacted, err := c.srv.Store.Compacted(); err != nil || compacted < 2 {
		t.Fatalf("the change log did not move past revision 1 (compacted %d, %v)", compacted, err)
	}
}

// TestWatchStreamFallsBehind: a watcher that is still reading an old revision
// when the log is compacted past it gets one ERROR event with a 410 Status and
// the stream ends.
func TestWatchStreamFallsBehind(t *testing.T) {
	c := newPrunedClient(t)
	c.want(c.do("POST", pods, podBody), 201)
	ts := httptest.NewServer(c.srv)
	t.Cleanup(ts.Close)
	s := openStream(t, ts.URL, pods+"?watch=true&resourceVersion="+listRV(t, c), "")

	// One transaction, five objects: the log is compacted past the cursor in a
	// single step, so the watcher cannot catch up between writes.
	res := podsRes()
	err := c.srv.Store.Write(false, func(tx *store.Tx) error {
		for i := 0; i < 5; i++ {
			p := &apis.Pod{}
			p.Metadata.Name = fmt.Sprintf("bg-%d", i)
			p.Metadata.Namespace = "default"
			p.Spec.Containers = []apis.Container{{Name: "c", Image: "nginx:1.27"}}
			stamp(res, p)
			if _, err := tx.Create(put(res, p)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("background writes: %v", err)
	}

	ev := s.next(5 * time.Second)
	if evType(ev) != "ERROR" {
		t.Fatalf("the watcher got %s, want an ERROR event", evType(ev))
	}
	obj := evObject(ev)
	if obj["code"] != float64(http.StatusGone) || obj["reason"] != "Expired" {
		t.Fatalf("the ERROR event is not a 410 Status: %v", obj)
	}
	s.end(5 * time.Second)
}

// TestWatchStreamTimeout: timeoutSeconds ends the stream by itself.
func TestWatchStreamTimeout(t *testing.T) {
	c, base := streamServer(t)
	c.want(c.do("POST", pods, podBody), 201)
	s := openStream(t, base, pods+"?watch=true&resourceVersion="+listRV(t, c)+"&timeoutSeconds=1", "")
	start := time.Now()
	s.end(5 * time.Second)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("timeoutSeconds=1 took %v", d)
	}
}

// TestWatchStreamFutureRevision: a revision the server has not reached yet is a
// 504, not a hang.
func TestWatchStreamFutureRevision(t *testing.T) {
	c := newClient(t)
	ahead := c.rev() + 10
	r := c.want(c.do("GET", fmt.Sprintf("%s?watch=true&resourceVersion=%d", pods, ahead), ""), 504)
	if r.at("reason") != "Timeout" {
		t.Fatalf("want reason Timeout: %s", r.raw)
	}
}

// TestWatchStreamBadQueries: the combinations the API server refuses.
func TestWatchStreamBadQueries(t *testing.T) {
	c := newClient(t)
	for _, q := range []string{
		"watch=true&sendInitialEvents=true",
		"watch=true&resourceVersion=0&sendInitialEvents=true&resourceVersionMatch=Exact",
		"watch=true&sendInitialEvents=maybe",
		"watch=true&resourceVersionMatch=NotOlderThan",
		"watch=true&timeoutSeconds=-1",
		"watch=true&timeoutSeconds=x",
		"watch=true&resourceVersion=abc",
	} {
		if r := c.do("GET", pods+"?"+q, ""); r.code != 400 {
			t.Errorf("%s answered HTTP %d, want 400: %s", q, r.code, r.raw)
		}
	}
}

// TestWatchStreamAcceptsWatchOne: kubectl sends watch=1 as well as watch=true.
func TestWatchStreamAcceptsWatchOne(t *testing.T) {
	c, base := streamServer(t)
	c.want(c.do("POST", pods, podBody), 201)
	s := openStream(t, base, pods+"?watch=1", "")
	if ev := s.next(3 * time.Second); evType(ev) != "ADDED" || evName(ev) != "p1" {
		t.Fatalf("watch=1 sent %s of %q, want ADDED p1", evType(ev), evName(ev))
	}
}

// TestWatchStreamTable: with Accept: Table the event object is a one-row Table,
// and only the first event carries the columns.
func TestWatchStreamTable(t *testing.T) {
	c, base := streamServer(t)
	c.want(c.do("POST", pods, podBody), 201)
	const accept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json"
	s := openStream(t, base, pods+"?watch=true", accept)
	ev := s.next(3 * time.Second)
	if evType(ev) != "ADDED" {
		t.Fatalf("the first event is %s, want ADDED", evType(ev))
	}
	obj := evObject(ev)
	if obj["kind"] != "Table" {
		t.Fatalf("the event object is %v, want a Table", obj["kind"])
	}
	cols, _ := obj["columnDefinitions"].([]any)
	if len(cols) != len(tableColumns["Pod"]) {
		t.Fatalf("the first Table has %d columns, want %d", len(cols), len(tableColumns["Pod"]))
	}
	rows, _ := obj["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("the first Table has %d rows, want 1", len(rows))
	}
	c.want(c.do("POST", pods, otherPod("p2")), 201)
	obj = evObject(s.next(3 * time.Second))
	if obj["kind"] != "Table" || obj["columnDefinitions"] != nil {
		t.Fatalf("only the first Table of a stream carries columnDefinitions: %v", obj)
	}
}

// TestWatchStreamNamespaceScope: a namespaced watch sees its namespace only, the
// cluster-wide one sees every namespace.
func TestWatchStreamNamespaceScope(t *testing.T) {
	c, base := streamServer(t)
	c.want(c.do("POST", "/api/v1/namespaces", `{"metadata":{"name":"team-a"}}`), 201)
	c.want(c.do("POST", pods, podBody), 201)
	c.want(c.do("POST", "/api/v1/namespaces/team-a/pods", otherPod("p2")), 201)

	s := openStream(t, base, pods+"?watch=true", "")
	if ev := s.next(3 * time.Second); evName(ev) != "p1" {
		t.Fatalf("the default namespace watch sent %q", evName(ev))
	}
	s.quiet(300 * time.Millisecond) // p2 lives in team-a

	all := openStream(t, base, "/api/v1/pods?watch=true", "")
	var got []string
	for range 2 {
		got = append(got, evName(all.next(3*time.Second)))
	}
	if strings.Join(got, ",") != "p1,p2" {
		t.Fatalf("the cluster-wide watch sent %v, want p1 and p2", got)
	}
}
