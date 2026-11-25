package api

// "What changed since revision N, and who did it" for agents and operators
// (design-agent-first-ha 2.3.2). The change log already holds every object
// version; this endpoint turns a slice of it into a few short lines: the
// fields that changed (never Secret values), who changed them, and a count of
// the status updates and heartbeats it folded away. An agent keeps only the
// `next` revision between calls.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/store"
)

// ChangesPath serves the change summary to every authenticated user; each
// entry is shown only to a caller that may list that resource there.
const ChangesPath = "/shearwater/changes"

const (
	defaultChangeLimit = 50
	maxChangeLimit     = 200
	maxChangeFields    = 6
	maxValueLen        = 60
)

// ChangeList is the response of ChangesPath.
type ChangeList struct {
	Revision int64        `json:"revision"`        // the store revision when read
	Since    *int64       `json:"since,omitempty"` // absent: the newest entries
	Next     int64        `json:"next"`            // pass as since to continue
	More     bool         `json:"more"`            // entries after next may already exist
	Changes  []ChangeItem `json:"changes"`
	// Folded counts entries left out of Changes: "status" (only status
	// fields changed) and "heartbeat" (only timestamps changed). Pass
	// status=true to include them.
	Folded map[string]int `json:"folded,omitempty"`
}

// ChangeItem is one change in a ChangeList.
type ChangeItem struct {
	Rev       int64    `json:"rev"`
	At        string   `json:"at"`
	Actor     string   `json:"actor"` // "system" when the change was not attributed
	Via       string   `json:"via,omitempty"`
	Type      string   `json:"type"` // ADDED, MODIFIED, DELETED
	Kind      string   `json:"kind"`
	APIGroup  string   `json:"apiGroup,omitempty"`
	Namespace string   `json:"namespace,omitempty"`
	Name      string   `json:"name"`
	Fields    []string `json:"fields,omitempty"`
}

// actorOf names the caller of a request for the change log: who (from the
// authenticated identity) and through what (kubectl or another API client).
func actorOf(r *http.Request) store.Actor {
	id, ok := auth.From(r.Context())
	if !ok || id.Name == "" {
		return store.Actor{}
	}
	via := "api"
	if strings.HasPrefix(r.Header.Get("User-Agent"), "kubectl/") {
		via = "kubectl"
	}
	return store.Actor{Name: id.Name, Via: via}
}

func (s *Server) serveChanges(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if r.Method != http.MethodGet {
		writeErr(w, errMethod(r.Method))
		return
	}
	list, err := s.changes(r, id)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			writeErr(w, ae)
		} else {
			writeErr(w, newErr(http.StatusInternalServerError, "InternalError", "INTERNAL", "safe", "%v", err))
		}
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) changes(r *http.Request, id auth.Identity) (ChangeList, error) {
	q := r.URL.Query()
	after := int64(-1)
	var since *int64
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return ChangeList{}, newErr(http.StatusBadRequest, "BadRequest", "INVALID", "never", "since must be a revision (a non-negative integer), got %q", v)
		}
		after, since = n, &n
	}
	limit := defaultChangeLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxChangeLimit {
			return ChangeList{}, newErr(http.StatusBadRequest, "BadRequest", "INVALID", "never", "limit must be 1 to %d, got %q", maxChangeLimit, v)
		}
		limit = n
	}
	namespace, actor := q.Get("namespace"), q.Get("actor")
	withStatus := q.Get("status") == "true" || q.Get("status") == "1"

	// Read more than limit: folded and invisible entries take no room in the
	// answer, but scanning stays bounded.
	scan := limit * 4
	entries, next, err := s.Store.History(after, store.HistoryFilter{Namespace: namespace, Actor: actor}, scan)
	if errors.Is(err, store.ErrCompacted) {
		rev, _ := s.Store.Rev()
		return ChangeList{}, newErr(http.StatusGone, "Expired", "COMPACTED", "never",
			"the change log no longer reaches back to revision %d", after).
			withHint("read the current state again, then ask for changes since revision %d", rev)
	}
	if err != nil {
		return ChangeList{}, err
	}
	rev, err := s.Store.Rev()
	if err != nil {
		return ChangeList{}, err
	}
	out := ChangeList{Revision: rev, Since: since, Next: next, Changes: []ChangeItem{}}
	if after >= 0 && len(entries) == scan {
		out.More = true
	}
	allowed := map[string]bool{}
	visible := func(e store.Entry) bool {
		if id.IsAdmin() {
			return true
		}
		k := e.Key.Group + "/" + e.Key.Resource + "/" + e.Key.Namespace
		ok, seen := allowed[k]
		if !seen {
			a := auth.Attributes{ResourceRequest: true, Verb: "list", APIGroup: e.Key.Group, APIVersion: versionOf(e.Key),
				Resource: e.Key.Resource, Namespace: e.Key.Namespace}
			ok = s.authorize(id, a) == nil
			allowed[k] = ok
		}
		return ok
	}
	folded := map[string]int{}
	// item returns the entry as shown, or false when it is left out.
	item := func(e store.Entry) (ChangeItem, bool) {
		if !visible(e) {
			return ChangeItem{}, false
		}
		name := e.Actor.Name
		if name == "" {
			name = "system"
		}
		if actor != "" && name != actor {
			return ChangeItem{}, false
		}
		fields, class := summarizeChange(e)
		if class != "" && !withStatus {
			folded[class]++
			return ChangeItem{}, false
		}
		return ChangeItem{Rev: e.Rev, At: e.At, Actor: name, Via: e.Actor.Via, Type: e.Type, Kind: kindOf(e.Key),
			APIGroup: e.Key.Group, Namespace: e.Key.Namespace, Name: e.Key.Name, Fields: fields}, true
	}
	if after < 0 {
		// The newest limit entries that are shown, oldest first.
		for i := len(entries) - 1; i >= 0 && len(out.Changes) < limit; i-- {
			if c, ok := item(entries[i]); ok {
				out.Changes = append(out.Changes, c)
			}
		}
		slices.Reverse(out.Changes)
	} else {
		for _, e := range entries {
			if len(out.Changes) == limit {
				// Stop at the last entry shown; the caller continues after it.
				out.Next, out.More = out.Changes[len(out.Changes)-1].Rev, true
				break
			}
			if c, ok := item(e); ok {
				out.Changes = append(out.Changes, c)
			}
		}
	}
	if len(folded) > 0 {
		out.Folded = folded
	}
	return out, nil
}

func resourceOf(k store.Key) *apis.Resource {
	for _, r := range apis.Resources {
		if r.Group == k.Group && r.Plural == k.Resource {
			return r
		}
	}
	return nil
}

func kindOf(k store.Key) string {
	if r := resourceOf(k); r != nil {
		return r.Kind
	}
	return k.Resource
}

func versionOf(k store.Key) string {
	if r := resourceOf(k); r != nil {
		return r.Version
	}
	return ""
}

// summarizeChange lists the fields a change touched as short "path old→new"
// lines. class is "status" when only status fields changed and "heartbeat"
// when only timestamps did; such entries are folded by default.
func summarizeChange(e store.Entry) (fields []string, class string) {
	if e.Type != "MODIFIED" || len(e.Prev) == 0 {
		return nil, ""
	}
	var before, after any
	if json.Unmarshal(e.Prev, &before) != nil || json.Unmarshal(e.Data, &after) != nil {
		return []string{"(not JSON)"}, ""
	}
	d := differ{secret: e.Key.Group == "" && e.Key.Resource == "secrets"}
	d.walk("", before, after)
	if len(d.lines) == 0 {
		return nil, "heartbeat"
	}
	statusOnly := true
	for _, p := range d.paths {
		if p != "status" && !strings.HasPrefix(p, "status.") && !strings.HasPrefix(p, "status[") {
			statusOnly = false
			break
		}
	}
	lines := d.lines
	if len(lines) > maxChangeFields {
		lines = append(lines[:maxChangeFields:maxChangeFields], fmt.Sprintf("+%d more", len(d.lines)-maxChangeFields))
	}
	if statusOnly {
		return lines, "status"
	}
	return lines, ""
}

type differ struct {
	secret bool // a Secret: say which keys changed, never their values
	paths  []string
	lines  []string
}

// ignored paths never appear: bookkeeping that changes with every write, and
// kubectl's copy of the whole applied object (it may contain Secret data).
func ignoredPath(p string) bool {
	switch p {
	case "metadata.resourceVersion", "metadata.managedFields", "metadata.generation",
		`metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]`:
		return true
	}
	return false
}

// timestampKey: values of these keys move without anything else changing
// (heartbeats, lease renewals, probe times).
func timestampKey(key string) bool {
	return strings.HasSuffix(key, "Time") || strings.HasSuffix(key, "Timestamp")
}

func (d *differ) walk(path string, a, b any) {
	if ignoredPath(path) {
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			break
		}
		keys := make([]string, 0, len(av)+len(bv))
		for k := range av {
			keys = append(keys, k)
		}
		for k := range bv {
			if _, dup := av[k]; !dup {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			x, inA := av[k]
			y, inB := bv[k]
			p := joinPath(path, k)
			switch {
			case inA && inB:
				if timestampKey(k) && isScalar(x) && isScalar(y) {
					continue
				}
				d.walk(p, x, y)
			case inB:
				if !ignoredPath(p) {
					d.add(p, "+"+p+"="+d.show(p, y))
				}
			default:
				if !ignoredPath(p) {
					d.add(p, "-"+p)
				}
			}
		}
		return
	case []any:
		bv, ok := b.([]any)
		if !ok {
			break
		}
		if len(av) != len(bv) {
			d.add(path, fmt.Sprintf("%s %d→%d items", path, len(av), len(bv)))
			return
		}
		for i := range av {
			d.walk(path+"["+elementName(av[i], bv[i], i)+"]", av[i], bv[i])
		}
		return
	}
	if equalJSON(a, b) {
		return
	}
	if d.secret && secretValuePath(path) {
		d.add(path, path+" changed")
		return
	}
	d.add(path, path+" "+d.show(path, a)+"→"+d.show(path, b))
}

func (d *differ) add(path, line string) {
	d.paths = append(d.paths, path)
	d.lines = append(d.lines, line)
}

func (d *differ) show(path string, v any) string {
	if d.secret && secretValuePath(path) {
		return "(hidden)"
	}
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return clip(x)
	case map[string]any:
		return fmt.Sprintf("{%d keys}", len(x))
	case []any:
		return fmt.Sprintf("[%d items]", len(x))
	}
	b, _ := json.Marshal(v)
	return clip(string(b))
}

func secretValuePath(p string) bool {
	return p == "data" || p == "stringData" || strings.HasPrefix(p, "data.") || strings.HasPrefix(p, "stringData.") ||
		strings.HasPrefix(p, "data[") || strings.HasPrefix(p, "stringData[")
}

// joinPath writes keys that are not plain identifiers in brackets, as
// annotation and label keys usually are.
func joinPath(path, key string) string {
	plain := key != ""
	for _, c := range key {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			plain = false
			break
		}
	}
	switch {
	case !plain:
		return path + `["` + key + `"]`
	case path == "":
		return key
	}
	return path + "." + key
}

// elementName prefers a list element's name (containers, ports, env) to
// its position, so a line reads containers[web].image.
func elementName(a, b any, i int) string {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		if an, ok := am["name"].(string); ok && an != "" {
			if bn, _ := bm["name"].(string); bn == an {
				return an
			}
		}
	}
	return strconv.Itoa(i)
}

func isScalar(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return false
	}
	return true
}

func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return slices.Equal(x, y)
}

func clip(s string) string {
	if len(s) <= maxValueLen {
		return s
	}
	cut := maxValueLen
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
