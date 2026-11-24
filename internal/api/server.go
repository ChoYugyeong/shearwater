// Package api is the HTTP face of Shearwater: the part of the Kubernetes API
// that kubectl needs for Pods, Deployments and Services (design 3).
package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"shearwater/internal/admission"
	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/operation"
	"shearwater/internal/store"
)

// Paths outside the Kubernetes API.
const (
	// AdminPrefix serves administrator-only Shearwater endpoints (users,
	// join tokens). Handlers are registered on Server.Admin.
	AdminPrefix = "/shearwater/admin/"
	// NodePrefix serves worker nodes. Server.NodeAPI authenticates them itself
	// because joining uses a join token, not an identity the API knows yet.
	NodePrefix = "/shearwater/node/"
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
	Version    = "0.1.0-alpha.15"
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
	// Operations runs delegated app operations. Nil selects Shearwater's own
	// engine; `sw compose serve` supplies a Docker Compose backend.
	Operations operation.Backend

	// Auth identifies requests. Nil accepts only Token (the administrator).
	Auth *auth.Authenticator
	// Authorizer judges Kubernetes API requests of identities that are not
	// administrators. Nil refuses all of them.
	Authorizer auth.Authorizer
	// Admit checks creates and updates after validation, inside the write
	// transaction (quota, pod security, RBAC escalation, host claims).
	Admit admission.Func
	// Admin serves AdminPrefix for administrators; NodeAPI serves NodePrefix.
	Admin, NodeAPI http.Handler
	// RemoteLogs streams the log of a container on another node (nil: only
	// this server's own node has logs). The request is already checked.
	RemoteLogs func(w http.ResponseWriter, r *http.Request, pod *apis.Pod, container string, attempt int, tail, limit int64, follow bool) error

	traceMu       sync.Mutex
	openapi       sync.Once
	oaDocs        map[string][]byte // "api/v1" -> document
	oaRoot        []byte
	operationInit sync.Once
	operationErr  error
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
	if p == operation.Prefix || strings.HasPrefix(p, operation.Prefix+"/") {
		s.serveOperation(w, r)
		return
	}
	if strings.HasPrefix(p, NodePrefix) {
		if s.NodeAPI == nil {
			writeErr(w, errPath(p))
			return
		}
		s.NodeAPI.ServeHTTP(w, r)
		return
	}
	id, ok := s.identify(r)
	if !ok {
		writeErr(w, newErr(http.StatusUnauthorized, "Unauthorized", "UNAUTHORIZED", "never", "missing or wrong bearer token").
			withHint("use the kubeconfig printed by `sw kubeconfig`, or one from `sw user add`"))
		return
	}
	if id.Kind == auth.KindNode || id.Kind == auth.KindJoin {
		// Nodes speak only the node API; a join token only joins.
		writeErr(w, newErr(http.StatusForbidden, "Forbidden", "FORBIDDEN", "never",
			"%s credentials are only valid on %s", id.Kind, NodePrefix))
		return
	}
	r = r.WithContext(auth.WithIdentity(r.Context(), id))
	if strings.HasPrefix(p, AdminPrefix) {
		switch {
		case !id.IsAdmin():
			writeErr(w, newErr(http.StatusForbidden, "Forbidden", "FORBIDDEN", "never",
				"User %q cannot use %s: it is for the administrator", id.Name, AdminPrefix).
				withHint("run this command with the administrator's kubeconfig (`sw kubeconfig`)"))
		case s.Admin == nil:
			writeErr(w, errPath(p))
		default:
			s.Admin.ServeHTTP(w, r)
		}
		return
	}
	switch {
	case p == "/api":
		s.serveAPIVersions(w)
	case p == "/apis":
		s.serveGroupList(w)
	case p == "/openapi/v2":
		s.serveOpenAPIV2(w, r)
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

// identify says who sent r.
func (s *Server) identify(r *http.Request) (auth.Identity, bool) {
	if s.Auth != nil {
		return s.Auth.Authenticate(r)
	}
	if s.authorized(r) {
		return auth.Admin(), true
	}
	return auth.Identity{}, false
}

// verbOf maps a request to the verb RBAC rules name.
func verbOf(r *http.Request, ri reqInfo) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if v := r.URL.Query().Get("watch"); v == "1" || v == "true" {
			return "watch"
		}
		if ri.name == "" {
			return "list"
		}
		return "get"
	case http.MethodPost:
		return "create"
	case http.MethodPut:
		return "update"
	case http.MethodPatch:
		return "patch"
	case http.MethodDelete:
		if ri.name == "" {
			return "deletecollection"
		}
		return "delete"
	}
	return strings.ToLower(r.Method)
}

// attributesOf describes a resource request for the authorizer.
func attributesOf(r *http.Request, ri reqInfo) auth.Attributes {
	a := auth.Attributes{ResourceRequest: true, Verb: verbOf(r, ri), APIGroup: ri.group, APIVersion: ri.version,
		Resource: ri.resource, Subresource: ri.sub, Namespace: ri.namespace, Name: ri.name}
	if a.Name == "" && (a.Verb == "list" || a.Verb == "watch") {
		// kubectl get NAME --watch lists with a name field selector; RBAC
		// rules that name objects should still match, as in Kubernetes.
		if v, ok := strings.CutPrefix(r.URL.Query().Get("fieldSelector"), "metadata.name="); ok && !strings.ContainsAny(v, ",=!") {
			a.Name = v
		}
	}
	return a
}

// authorize refuses what the identity may not do. Administrators skip it.
func (s *Server) authorize(id auth.Identity, a auth.Attributes) *apiError {
	if id.IsAdmin() {
		return nil
	}
	reason := "no authorizer is configured, so only the administrator may use the API"
	if s.Authorizer != nil {
		var allowed bool
		if allowed, reason = s.Authorizer.Authorize(id, a); allowed {
			return nil
		}
	}
	var msg string
	if a.ResourceRequest {
		res := a.Resource
		if a.Subresource != "" {
			res += "/" + a.Subresource
		}
		scope := "at the cluster scope"
		if a.Namespace != "" {
			scope = fmt.Sprintf("in the namespace %q", a.Namespace)
		}
		subject := res
		if a.Name != "" {
			subject = fmt.Sprintf("%s %q", res, a.Name)
		}
		msg = fmt.Sprintf("%s is forbidden: User %q cannot %s resource %q in API group %q %s", subject, id.Name, a.Verb, res, a.APIGroup, scope)
	} else {
		msg = fmt.Sprintf("forbidden: User %q cannot get path %q", id.Name, a.Path)
	}
	if reason != "" {
		msg += ": " + reason
	}
	e := newErr(http.StatusForbidden, "Forbidden", "FORBIDDEN", "never", "%s", msg).
		withHint("ask the administrator for a RoleBinding (`sw user add` can create one), or check with `kubectl auth can-i --list`")
	if a.ResourceRequest {
		e.details = &apis.StatusDetails{Name: a.Name, Group: a.APIGroup, Kind: a.Resource}
	}
	return e
}

// admit runs the admission plugins. An error from a plugin is returned as it
// should reach the client.
func (s *Server) admit(req admission.Request) error {
	if s.Admit == nil {
		return nil
	}
	err := s.Admit(req)
	var d *admission.Denied
	if errors.As(err, &d) {
		retry := "never"
		return newErr(d.Code, d.Reason, d.ErrCode, retry, "%s", d.Message).withHint("%s", d.Hint)
	}
	return err
}

// reqInfo is a parsed resource path.
type reqInfo struct {
	group, version string
	namespace      string
	resource       string
	name           string
	sub            string
	id             auth.Identity // who asked (set by serveAPI)
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
	ri.id, _ = auth.From(r.Context())
	if e := s.authorize(ri.id, attributesOf(r, ri)); e != nil {
		writeErr(w, e)
		return
	}

	var err error
	switch {
	case res.Group == apis.AuthorizationGroup || res.Group == apis.AuthenticationGroup:
		err = s.review(w, r, res, ri)
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
	case secretPath(r.URL.Path):
		// Secret values must not reach the development trace file.
		entry["bodyRedacted"] = len(body)
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

func secretPath(p string) bool {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	return len(segs) >= 3 && segs[0] == "api" && slices.Contains(segs[2:], "secrets")
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
