// Package api is the HTTP face of Shearwater: the part of the Kubernetes API
// that kubectl needs for Pods, Deployments and Services (design 3).
package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

// Version is the product release version, without the Git tag's leading "v".
// This is the single source for CLI/help, server logs and Kubernetes metadata.
// Bump it before a release; hack/check_version.py compares the built executable
// with the intended tag so an old value cannot silently ship under a new tag.
//
// GitVersion starts with the Kubernetes API level, not the product version:
// kubectl uses that prefix for compatibility checks. Our product version belongs
// in build metadata; releasing Shearwater does not change the API level.
const (
	Version    = "0.1.0-alpha.3-dev"
	GitVersion = "v1.36.0+shearwater." + Version
)

const maxBody = 3 << 20

// timeNow is the clock for the AGE column; tests replace it.
var timeNow = time.Now

type Server struct {
	Store *store.Store
	Token string    // admin bearer token
	Trace io.Writer // when set, every request is appended as one JSON line (development aid)

	// NodeName is the node whose agent runs in this process ("" when there is
	// none). Deleting a pod bound to it is graceful: the pod is marked and the
	// agent lets the object go once the containers are gone (design 6).
	NodeName string
	// LogDir is where the node agent has container logs written (`kubectl logs`).
	LogDir string

	traceMu sync.Mutex
	openapi sync.Once
	oaDocs  map[string][]byte // "api/v1" -> document
	oaRoot  []byte
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Trace != nil {
		s.traced(w, r)
		return
	}
	s.route(w, r)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch p {
	case "/healthz", "/readyz", "/livez":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok"))
		return
	case "/version":
		writeJSON(w, 200, map[string]string{
			"major": "1", "minor": "36", "gitVersion": GitVersion, "platform": "linux/amd64",
		})
		return
	}
	if !s.authorized(r) {
		writeErr(w, newErr(http.StatusUnauthorized, "Unauthorized", "UNAUTHORIZED", "never", "missing or wrong bearer token").
			withHint("use the kubeconfig printed by `sw kubeconfig`"))
		return
	}
	switch {
	case p == "/api":
		s.serveAPIVersions(w)
	case p == "/apis":
		s.serveGroupList(w)
	case p == "/openapi/v3" || strings.HasPrefix(p, "/openapi/v3/"):
		s.serveOpenAPI(w, r)
	case strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/apis/"):
		s.serveAPI(w, r)
	default:
		writeErr(w, errPath(p))
	}
}

func errPath(p string) *apiError {
	return newErr(http.StatusNotFound, "NotFound", "NOT_FOUND", "never", "the server has nothing at %s", p)
}

func (s *Server) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && s.Token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

// reqInfo is a parsed resource path.
type reqInfo struct {
	group, version string
	namespace      string
	resource       string
	name           string
	sub            string
}

// serveAPI handles everything under /api/ and /apis/.
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	var ri reqInfo
	var rest []string
	switch {
	case segs[0] == "api" && len(segs) >= 2:
		ri.version, rest = segs[1], segs[2:]
	case segs[0] == "apis" && len(segs) == 2:
		s.serveGroup(w, segs[1])
		return
	case segs[0] == "apis" && len(segs) >= 3:
		ri.group, ri.version, rest = segs[1], segs[2], segs[3:]
	default:
		writeErr(w, errPath(r.URL.Path))
		return
	}
	if len(rest) == 0 {
		s.serveResourceList(w, ri.group, ri.version)
		return
	}
	// /namespaces/{ns}/{resource}...; /namespaces/{name}/status|finalize belongs to the namespace itself.
	if rest[0] == "namespaces" && len(rest) >= 3 && !(ri.group == "" && len(rest) == 3 && (rest[2] == "status" || rest[2] == "finalize")) {
		ri.namespace, rest = rest[1], rest[2:]
	}
	if len(rest) > 3 {
		writeErr(w, errPath(r.URL.Path))
		return
	}
	ri.resource = rest[0]
	if len(rest) > 1 {
		ri.name = rest[1]
	}
	if len(rest) > 2 {
		if ri.name == "" {
			writeErr(w, errPath(r.URL.Path))
			return
		}
		ri.sub = rest[2]
	}
	res := apis.Lookup(ri.group, ri.version, ri.resource)
	if res == nil || (!res.Namespaced && ri.namespace != "") || (res.Namespaced && ri.namespace == "" && ri.name != "") {
		writeErr(w, errPath(r.URL.Path))
		return
	}

	var err error
	switch {
	case ri.name == "":
		switch r.Method {
		case http.MethodGet:
			err = s.list(w, r, res, ri)
		case http.MethodPost:
			err = s.create(w, r, res, ri)
		case http.MethodDelete:
			err = errUnsupported(http.StatusMethodNotAllowed, "MethodNotAllowed", "deleting a whole collection is not supported").
				withHint("delete objects by name, for example `kubectl delete %s NAME`", res.Plural)
		default:
			err = errMethod(r.Method)
		}
	case ri.sub == "log" && res.Group == "" && res.Plural == "pods":
		if r.Method == http.MethodGet {
			err = s.podLog(w, r, res, ri)
		} else {
			err = errMethod(r.Method)
		}
	case ri.sub == "" || (ri.sub == "status" && res.StatusSub):
		switch r.Method {
		case http.MethodGet:
			err = s.get(w, r, res, ri)
		case http.MethodPut:
			err = s.update(w, r, res, ri)
		case http.MethodDelete:
			if ri.sub != "" {
				err = errMethod(r.Method)
			} else {
				err = s.delete(w, r, res, ri)
			}
		case http.MethodPatch:
			err = s.patch(w, r, res, ri)
		default:
			err = errMethod(r.Method)
		}
	case ri.sub == "scale" && res.ScaleSub:
		switch r.Method {
		case http.MethodGet:
			err = s.getScale(w, res, ri)
		case http.MethodPut, http.MethodPatch:
			err = s.updateScale(w, r, res, ri)
		default:
			err = errMethod(r.Method)
		}
	default:
		err = errPath(r.URL.Path)
	}
	if err != nil {
		writeErr(w, toAPIError(err, res, ri.name))
	}
}

func errMethod(m string) *apiError {
	return errUnsupported(http.StatusMethodNotAllowed, "MethodNotAllowed", "method %s is not supported here", m)
}

// ---- request trace (development aid; off unless Server.Trace is set) ----

type statusRecorder struct {
	http.ResponseWriter
	code int
}

// Unwrap lets http.ResponseController reach Flush on the real writer (watch).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) traced(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	r.Body = io.NopCloser(bytes.NewReader(body))
	rec := &statusRecorder{ResponseWriter: w, code: 200}
	s.route(rec, r)

	entry := map[string]any{
		"at": time.Now().UTC().Format(time.RFC3339), "method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery,
		"contentType": r.Header.Get("Content-Type"), "accept": r.Header.Get("Accept"),
		"userAgent": r.Header.Get("User-Agent"), "hasToken": r.Header.Get("Authorization") != "", "status": rec.code,
	}
	switch {
	case len(body) == 0:
	case utf8.Valid(body) && !bytes.ContainsRune(body, 0):
		entry["body"] = string(body)
	default:
		entry["bodyBase64"] = base64.StdEncoding.EncodeToString(body)
	}
	line, _ := json.Marshal(entry)
	s.traceMu.Lock()
	s.Trace.Write(append(line, '\n'))
	s.traceMu.Unlock()
}

// Bootstrap creates the namespaces every cluster starts with. It writes
// nothing when they already exist.
func (s *Server) Bootstrap() error {
	res := apis.Lookup("", "v1", "namespaces")
	return s.Store.Write(false, func(tx *store.Tx) error {
		for _, name := range []string{"default", "kube-system"} {
			key := store.Key{Resource: res.Plural, Name: name}
			if _, err := tx.Get(key); err == nil {
				continue
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			ns := &apis.Namespace{}
			ns.Metadata.Name = name
			stamp(res, ns)
			if _, err := tx.Create(put(res, ns)); err != nil {
				return err
			}
		}
		return nil
	})
}
