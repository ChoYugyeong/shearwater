package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

// Watch (design 3.2 and 5). A watcher holds nothing but a cursor: the last
// revision it has handled. The store's hub only says "something was
// committed"; the watcher then reads the change log after its cursor, in short
// batches, and writes one JSON line per event. A slow client therefore costs no
// memory and never blocks a write; if it falls behind the compacted part of the
// log it gets an ERROR event with a 410 Status and must list again.

const (
	watchBatch       = 200
	bookmarkInterval = time.Minute
	initialEventsEnd = "k8s.io/initial-events-end"
)

// filter is the selector of a list or watch request.
type filter struct {
	labels apis.Selector
	fields []apis.FieldRequirement
}

func parseFilter(q url.Values, res *apis.Resource) (filter, error) {
	sel, err := apis.ParseSelector(q.Get("labelSelector"))
	if err != nil {
		return filter{}, errBadRequest("%v", err)
	}
	fsel, err := apis.ParseFieldSelector(q.Get("fieldSelector"), res.SelectableFields())
	if err != nil {
		return filter{}, errBadRequest("%v", err)
	}
	return filter{labels: sel, fields: fsel}, nil
}

func (f filter) matches(data []byte) (bool, error) {
	if f.labels.Empty() && len(f.fields) == 0 {
		return true, nil
	}
	labels, fields, err := apis.Selectable(data)
	if err != nil {
		return false, fmt.Errorf("a stored object is not readable: %v", err)
	}
	return f.labels.Matches(labels) && apis.MatchFields(f.fields, fields), nil
}

// event decides what a watcher with this filter sees of one change. With a
// selector, the type follows whether the object matched before and after:
//
//	no  -> yes   ADDED     the new object
//	yes -> yes   MODIFIED  the new object
//	yes -> no    DELETED   the object as it was, carrying this change's revision
//	no  -> no    nothing
//
// typ is "" when there is nothing to send.
func (f filter) event(c store.Change) (typ string, data []byte, err error) {
	was, is := false, false
	if c.Prev != nil {
		if was, err = f.matches(c.Prev); err != nil {
			return "", nil, err
		}
	}
	if c.Type != "DELETED" {
		if is, err = f.matches(c.Data); err != nil {
			return "", nil, err
		}
	}
	switch {
	case is && was:
		return "MODIFIED", c.Data, nil
	case is:
		return "ADDED", c.Data, nil
	case was && c.Type == "DELETED":
		return "DELETED", c.Data, nil
	case was:
		return "DELETED", withResourceVersion(c.Prev, c.Rev), nil
	}
	return "", nil, nil
}

// withResourceVersion returns the stored object with another resourceVersion.
// Stored bytes come from json.Marshal of our own types, where
// "resourceVersion":"N" appears first inside metadata; a quote inside a string
// value is always escaped, so the first unescaped match is that member.
func withResourceVersion(data []byte, rev int64) []byte {
	const key = `"resourceVersion":"`
	i := bytes.Index(data, []byte(key))
	if i >= 0 {
		start := i + len(key)
		if end := bytes.IndexByte(data[start:], '"'); end >= 0 {
			out := make([]byte, 0, len(data)+8)
			out = append(out, data[:start]...)
			out = strconv.AppendInt(out, rev, 10)
			return append(out, data[start+end:]...)
		}
	}
	var v map[string]any
	if json.Unmarshal(data, &v) == nil {
		if m, ok := v["metadata"].(map[string]any); ok {
			m["resourceVersion"] = strconv.FormatInt(rev, 10)
			if b, err := json.Marshal(v); err == nil {
				return b
			}
		}
	}
	return data
}

func errExpired(requested, compacted int64) *apiError {
	return newErr(http.StatusGone, "Expired", "EXPIRED", "safe", "too old resource version: %d (the change log starts after %d)", requested, compacted).
		withHint("list again and watch from the list's resourceVersion")
}

// watchStream writes events to one client.
type watchStream struct {
	w        http.ResponseWriter
	rc       *http.ResponseController
	res      *apis.Resource
	table    *tableOpts
	sentCols bool
	buf      bytes.Buffer
}

func (ws *watchStream) send(typ string, data []byte) error {
	ws.buf.Reset()
	ws.buf.WriteString(`{"type":"` + typ + `","object":`)
	if ws.table != nil && typ != "ERROR" {
		rev := ""
		var head struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if json.Unmarshal(data, &head) == nil {
			rev = head.Metadata.ResourceVersion
		}
		t := newTable(ws.res, rev, !ws.sentCols)
		ws.sentCols = true
		if typ != "BOOKMARK" {
			row, err := tableRowFor(ws.res, data, ws.table, time.Now())
			if err != nil {
				return err
			}
			t.Rows = append(t.Rows, row)
		}
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		ws.buf.Write(b)
	} else {
		ws.buf.Write(data)
	}
	ws.buf.WriteString("}\n")
	_, err := ws.w.Write(ws.buf.Bytes())
	return err
}

func (ws *watchStream) bookmark(rev int64, initialEnd bool) error {
	ann := ""
	if initialEnd {
		ann = `,"annotations":{"` + initialEventsEnd + `":"true"}`
	}
	return ws.send("BOOKMARK", fmt.Appendf(nil, `{"kind":%q,"apiVersion":%q,"metadata":{"resourceVersion":"%d"%s}}`,
		ws.res.Kind, ws.res.APIVersion(), rev, ann))
}

func (ws *watchStream) fail(e *apiError) {
	b, _ := json.Marshal(e.status())
	ws.send("ERROR", b)
	ws.rc.Flush()
}

func (s *Server) watch(w http.ResponseWriter, r *http.Request, res *apis.Resource, ri reqInfo, f filter, tbl *tableOpts) error {
	q := r.URL.Query()
	initial := q.Get("sendInitialEvents")
	switch initial {
	case "", "true", "false":
	default:
		return errBadRequest("sendInitialEvents must be true or false, got %q", initial)
	}
	switch match := q.Get("resourceVersionMatch"); {
	case initial == "true" && match != "NotOlderThan":
		return errBadRequest("sendInitialEvents=true needs resourceVersionMatch=NotOlderThan")
	case initial == "" && match != "":
		return errBadRequest("resourceVersionMatch applies to a list, or to a watch with sendInitialEvents")
	}
	bookmarks := q.Get("allowWatchBookmarks") == "true"
	var deadline <-chan time.Time
	if v := q.Get("timeoutSeconds"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return errBadRequest("timeoutSeconds must be a non-negative integer, got %q", v)
		}
		if n > 0 {
			t := time.NewTimer(time.Duration(n) * time.Second)
			defer t.Stop()
			deadline = t.C
		}
	}

	// Where to start. Without a resourceVersion (or with 0, or with
	// sendInitialEvents) the client gets the current objects as ADDED events
	// first; with one it gets exactly the changes after it.
	rvParam := q.Get("resourceVersion")
	sendState := initial == "true" || (initial == "" && (rvParam == "" || rvParam == "0"))
	var cursor int64
	var state []store.Record
	var err error
	switch {
	case sendState || rvParam == "" || rvParam == "0":
		if state, cursor, err = s.Store.List(res.Group, res.Plural, ri.namespace); err != nil {
			return err
		}
		if !sendState {
			state = nil
		}
	default:
		if cursor, err = strconv.ParseInt(rvParam, 10, 64); err != nil || cursor < 0 {
			return errBadRequest("resourceVersion must be a number from a list or watch response, got %q", rvParam)
		}
		now, err := s.Store.Rev()
		if err != nil {
			return err
		}
		if cursor > now {
			return newErr(http.StatusGatewayTimeout, "Timeout", "INVALID", "safe", "resourceVersion %d is ahead of the server (now at %d)", cursor, now).
				withHint("list again; the server's state may have been reset")
		}
		compacted, err := s.Store.Compacted()
		if err != nil {
			return err
		}
		if cursor < compacted {
			return errExpired(cursor, compacted)
		}
	}

	ws := &watchStream{w: w, rc: http.NewResponseController(w), res: res, table: tbl}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	for _, rec := range state {
		ok, err := f.matches(rec.Data)
		if err != nil {
			ws.fail(errInternal(err))
			return nil
		}
		if ok {
			if err := ws.send("ADDED", rec.Data); err != nil {
				return nil
			}
		}
	}
	if initial == "true" {
		if err := ws.bookmark(cursor, true); err != nil {
			return nil
		}
	}
	if err := ws.rc.Flush(); err != nil {
		return nil // the client is gone
	}

	var tick <-chan time.Time
	if bookmarks {
		t := time.NewTicker(bookmarkInterval)
		defer t.Stop()
		tick = t.C
	}
	for {
		changed := s.Store.Changed() // before reading, so a commit in between still wakes us
		for {
			changes, next, err := s.Store.Changes(res.Group, res.Plural, ri.namespace, cursor, watchBatch)
			if errors.Is(err, store.ErrCompacted) {
				compacted, _ := s.Store.Compacted()
				ws.fail(errExpired(cursor, compacted))
				return nil
			}
			if err != nil {
				ws.fail(errInternal(err))
				return nil
			}
			for _, c := range changes {
				typ, data, err := f.event(c)
				if err != nil {
					ws.fail(errInternal(err))
					return nil
				}
				if typ != "" {
					if err := ws.send(typ, data); err != nil {
						return nil
					}
				}
			}
			cursor = next
			if len(changes) > 0 {
				if err := ws.rc.Flush(); err != nil {
					return nil
				}
			}
			if len(changes) < watchBatch {
				break
			}
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return nil
		case <-deadline:
			return nil
		case <-tick:
			if ws.bookmark(cursor, false) != nil || ws.rc.Flush() != nil {
				return nil
			}
		}
	}
}
