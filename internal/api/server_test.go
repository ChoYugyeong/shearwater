package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"shearwater/internal/store"
)

type client struct {
	t   *testing.T
	srv *Server
}

type reply struct {
	code   int
	header http.Header
	raw    string
	body   map[string]any
}

// path digs into the decoded body: r.at("metadata", "name").
func (r reply) at(keys ...string) any {
	var v any = r.body
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func newClient(t *testing.T) *client {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := &Server{Store: st, Token: "secret"}
	if err := srv.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	return &client{t: t, srv: srv}
}

func (c *client) do(method, path, body string, headers ...string) reply {
	c.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	c.srv.ServeHTTP(w, req)
	r := reply{code: w.Code, header: w.Header(), raw: w.Body.String()}
	json.Unmarshal(w.Body.Bytes(), &r.body)
	return r
}

func (c *client) want(r reply, code int) reply {
	c.t.Helper()
	if r.code != code {
		c.t.Fatalf("got HTTP %d, want %d: %s", r.code, code, r.raw)
	}
	return r
}

func (c *client) rev() int64 {
	r, _ := c.srv.Store.Rev()
	return r
}

const (
	pods    = "/api/v1/namespaces/default/pods"
	deploys = "/apis/apps/v1/namespaces/default/deployments"
	podBody = `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p1","labels":{"app":"web"}},"spec":{"containers":[{"name":"c","image":"nginx:1.27"}]},"status":{"phase":"Running"}}`
)

func TestAuthAndHealth(t *testing.T) {
	c := newClient(t)
	c.want(c.do("GET", "/readyz", "", "Authorization", ""), 200)
	c.want(c.do("GET", "/version", "", "Authorization", ""), 200)
	r := c.want(c.do("GET", pods, "", "Authorization", "Bearer wrong"), 401)
	if r.at("reason") != "Unauthorized" || !strings.HasPrefix(r.at("message").(string), "UNAUTHORIZED retry=never: ") {
		t.Fatalf("a 401 must be a Status with the agent-facing first line: %s", r.raw)
	}
	c.want(c.do("GET", "/nope", ""), 404)
}

func TestDiscoveryAndOpenAPI(t *testing.T) {
	c := newClient(t)
	if r := c.want(c.do("GET", "/api", ""), 200); r.at("kind") != "APIVersions" {
		t.Fatal(r.raw)
	}
	r := c.want(c.do("GET", "/apis", ""), 200)
	if !strings.Contains(r.raw, `"groupVersion":"apps/v1"`) || !strings.Contains(r.raw, `"groupVersion":"discovery.k8s.io/v1"`) {
		t.Fatal(r.raw)
	}
	r = c.want(c.do("GET", "/api/v1", ""), 200)
	for _, want := range []string{`"name":"pods"`, `"name":"pods/status"`, `"name":"namespaces"`, `"shortNames":["svc"]`} {
		if !strings.Contains(r.raw, want) {
			t.Errorf("/api/v1 lacks %s", want)
		}
	}
	c.want(c.do("GET", "/apis/apps/v1", ""), 200)
	c.want(c.do("GET", "/apis/batch/v1", ""), 404)

	r = c.want(c.do("GET", "/openapi/v3", ""), 200)
	url, _ := r.at("paths", "apis/apps/v1", "serverRelativeURL").(string)
	if url == "" {
		t.Fatalf("/openapi/v3 must point at the apps/v1 document: %s", r.raw)
	}
	r = c.want(c.do("GET", url, ""), 200)
	op := r.at("paths", "/apis/apps/v1/namespaces/{namespace}/deployments/{name}", "patch")
	b, _ := json.Marshal(op)
	if !strings.Contains(string(b), `"name":"fieldValidation"`) || !strings.Contains(string(b), `"kind":"Deployment"`) {
		t.Fatalf("the patch operation must name the kind and the fieldValidation parameter: %s", b)
	}
	c.want(c.do("GET", "/openapi/v2", ""), 404)
}

func TestPodLifecycle(t *testing.T) {
	c := newClient(t)
	r := c.want(c.do("POST", pods+"?fieldValidation=Strict", podBody), 201)
	if r.at("status", "phase") != "Pending" {
		t.Errorf("status in a create body is ignored; a new pod is Pending: %v", r.at("status"))
	}
	if r.at("spec", "restartPolicy") != "Always" || r.at("metadata", "uid") == nil || r.at("metadata", "resourceVersion") != "3" ||
		r.at("metadata", "namespace") != "default" || r.at("metadata", "generation") != float64(1) || r.at("metadata", "creationTimestamp") == nil {
		t.Errorf("defaults or server-owned metadata are missing: %s", r.raw)
	}
	uid := r.at("metadata", "uid")

	if r := c.want(c.do("POST", pods, podBody), 409); r.at("reason") != "AlreadyExists" {
		t.Errorf("creating it twice: %s", r.raw)
	}
	if r := c.want(c.do("GET", pods+"/p1", ""), 200); r.at("metadata", "uid") != uid {
		t.Errorf("get: %s", r.raw)
	}
	c.want(c.do("GET", pods+"/missing", ""), 404)
	c.want(c.do("GET", "/api/v1/pods/p1", ""), 404) // a namespaced object has no cluster-wide item URL

	// list and selectors
	c.want(c.do("POST", pods, strings.ReplaceAll(strings.ReplaceAll(podBody, "p1", "p2"), `"web"`, `"db"`)), 201)
	items := func(path string) int {
		r := c.want(c.do("GET", path, ""), 200)
		return len(r.at("items").([]any))
	}
	if n := items(pods); n != 2 {
		t.Errorf("list: %d items", n)
	}
	if r := c.do("GET", pods, ""); r.at("kind") != "PodList" || r.at("metadata", "resourceVersion") != "4" {
		t.Errorf("list metadata: %s", r.raw)
	}
	if n := items(pods + "?labelSelector=app%3Dweb"); n != 1 {
		t.Errorf("labelSelector app=web: %d items", n)
	}
	if n := items(pods + "?labelSelector=app+in+(web,db)"); n != 2 {
		t.Errorf("labelSelector in: %d items", n)
	}
	if n := items(pods + "?fieldSelector=metadata.name%3Dp2"); n != 1 {
		t.Errorf("fieldSelector metadata.name: %d items", n)
	}
	if n := items(pods + "?fieldSelector=status.phase%3DRunning"); n != 0 {
		t.Errorf("fieldSelector status.phase: %d items", n)
	}
	if n := items("/api/v1/pods"); n != 2 {
		t.Errorf("all namespaces: %d items", n)
	}
	if n := items("/api/v1/namespaces/nowhere/pods"); n != 0 {
		t.Errorf("unknown namespace lists empty, got %d", n)
	}
	c.want(c.do("GET", pods+"?fieldSelector=spec.foo%3D1", ""), 400)
	c.want(c.do("GET", pods+"?labelSelector=a+b", ""), 400)

	// update: stale resourceVersion, image change, forbidden change
	stale := strings.Replace(podBody, `"name":"p1"`, `"name":"p1","resourceVersion":"1"`, 1)
	if r := c.want(c.do("PUT", pods+"/p1", stale), 409); r.at("reason") != "Conflict" || !strings.Contains(r.raw, "CONFLICT retry=safe") {
		t.Errorf("stale update: %s", r.raw)
	}
	r = c.want(c.do("PUT", pods+"/p1", strings.Replace(podBody, "nginx:1.27", "nginx:1.28", 1)), 200)
	if r.at("metadata", "generation") != float64(2) || r.at("metadata", "uid") != uid || r.at("status", "phase") != "Pending" {
		t.Errorf("an image change bumps generation and keeps uid and status: %s", r.raw)
	}
	before := c.rev()
	r = c.want(c.do("PUT", pods+"/p1", strings.Replace(podBody, "nginx:1.27", "nginx:1.28", 1)), 200)
	if c.rev() != before {
		t.Errorf("an update that changes nothing must not take a revision (%d -> %d)", before, c.rev())
	}
	c.want(c.do("PUT", pods+"/p1", strings.Replace(podBody, `"image":"nginx:1.27"`, `"image":"nginx:1.28","command":["sh"]`, 1)), 422)
	c.want(c.do("PUT", pods+"/missing", strings.ReplaceAll(podBody, "p1", "missing")), 404)
	c.want(c.do("PUT", pods+"/p1", strings.ReplaceAll(podBody, "p1", "other")), 400)

	// /status: replaces status only, generation stays
	st := `{"metadata":{"name":"p1"},"spec":{"containers":[]},"status":{"phase":"Running","podIP":"10.244.0.5"}}`
	r = c.want(c.do("PUT", pods+"/p1/status", st), 200)
	if r.at("status", "phase") != "Running" || r.at("metadata", "generation") != float64(2) || len(r.at("spec", "containers").([]any)) != 1 {
		t.Errorf("/status must change status only: %s", r.raw)
	}
	if n := items(pods + "?fieldSelector=status.phase%3DRunning"); n != 1 {
		t.Errorf("fieldSelector status.phase after status update: %d items", n)
	}

	// delete
	r = c.want(c.do("DELETE", pods+"/p1", `{"propagationPolicy":"Background","preconditions":{"uid":"nope"}}`), 409)
	r = c.want(c.do("DELETE", pods+"/p1", `{"propagationPolicy":"Background"}`), 200)
	if r.at("status") != "Success" || r.at("details", "uid") != uid {
		t.Errorf("delete must answer with the uid of what it removed: %s", r.raw)
	}
	c.want(c.do("GET", pods+"/p1", ""), 404)
	c.want(c.do("DELETE", pods+"/p1", ""), 404)
	c.want(c.do("DELETE", pods, ""), 405)
	c.want(c.do("PATCH", pods+"/p2", `{}`, "Content-Type", "application/merge-patch+json"), 200)
}

func TestFieldValidationModes(t *testing.T) {
	c := newClient(t)
	body := strings.Replace(podBody, `"spec":{`, `"spec":{"bogus":1,`, 1)
	r := c.want(c.do("POST", pods+"?fieldValidation=Strict", body), 400)
	if !strings.Contains(r.raw, `unknown field \"spec.bogus\"`) || r.at("reason") != "BadRequest" {
		t.Errorf("strict: %s", r.raw)
	}
	r = c.want(c.do("POST", pods, body), 201) // the default is Warn
	if w := r.header.Get("Warning"); !strings.Contains(w, "spec.bogus") || strings.Contains(r.raw, "bogus") {
		t.Errorf("warn must drop the field and say so: header %q body %s", w, r.raw)
	}
	r = c.want(c.do("POST", pods+"?fieldValidation=Ignore", strings.ReplaceAll(body, "p1", "p2")), 201)
	if r.header.Get("Warning") != "" {
		t.Errorf("ignore must stay silent: %q", r.header.Get("Warning"))
	}
	c.want(c.do("POST", pods+"?fieldValidation=Maybe", strings.ReplaceAll(body, "p1", "p3")), 400)
}

func TestCreateRefusals(t *testing.T) {
	c := newClient(t)
	r := c.want(c.do("POST", pods, `{"metadata":{"name":"p"},"spec":{"containers":[{"name":"c"}]}}`), 422)
	if r.at("reason") != "Invalid" || !strings.Contains(r.raw, "INVALID retry=never") || !strings.Contains(r.raw, "spec.containers[0].image") {
		t.Errorf("missing image: %s", r.raw)
	}
	if causes, _ := r.at("details", "causes").([]any); len(causes) != 1 {
		t.Errorf("details.causes must list each field: %s", r.raw)
	}
	r = c.want(c.do("POST", pods, `{"metadata":{"name":"p"},"spec":{"hostNetwork":true,"containers":[{"name":"c","image":"i"}]}}`), 422)
	if !strings.Contains(r.raw, "UNSUPPORTED retry=never") {
		t.Errorf("a rejected-grade field is UNSUPPORTED: %s", r.raw)
	}
	c.want(c.do("POST", pods, `{"kind":"Service","metadata":{"name":"p"}}`), 400)
	c.want(c.do("POST", pods, `{"metadata":{"name":"p","namespace":"other"},"spec":{"containers":[{"name":"c","image":"i"}]}}`), 400)
	c.want(c.do("POST", pods, `not json`), 400)
	c.want(c.do("POST", "/api/v1/namespaces/nowhere/pods", podBody), 404)
	c.want(c.do("POST", "/api/v1/pods", podBody), 405)
	c.want(c.do("POST", "/api/v1/nodes", `{"metadata":{"name":"n"}}`), 405)
	r = c.want(c.do("POST", deploys, "<x/>", "Content-Type", "application/xml"), 415)
	if !strings.Contains(r.raw, "application/json") {
		t.Errorf("a 415 must say what to send instead: %s", r.raw)
	}

	r = c.want(c.do("POST", pods, `{"metadata":{"generateName":"web-"},"spec":{"containers":[{"name":"c","image":"i"}]}}`), 201)
	if name, _ := r.at("metadata", "name").(string); len(name) != len("web-")+5 || !strings.HasPrefix(name, "web-") {
		t.Errorf("generateName: %q", name)
	}
}

func TestDryRunStoresNothing(t *testing.T) {
	c := newClient(t)
	before := c.rev()
	r := c.want(c.do("POST", pods+"?dryRun=All", podBody), 201)
	if r.at("metadata", "uid") == nil {
		t.Errorf("a dry run returns the object it would create: %s", r.raw)
	}
	c.want(c.do("GET", pods+"/p1", ""), 404)
	c.want(c.do("POST", pods, podBody), 201)
	mid := c.rev()
	c.want(c.do("DELETE", pods+"/p1?dryRun=All", ""), 200)
	c.want(c.do("PUT", pods+"/p1?dryRun=All", strings.Replace(podBody, "1.27", "1.28", 1)), 200)
	if r := c.want(c.do("GET", pods+"/p1", ""), 200); !strings.Contains(r.raw, "nginx:1.27") || c.rev() != mid || mid != before+1 {
		t.Errorf("dry-run delete and update must change nothing: rev %d -> %d -> %d, %s", before, mid, c.rev(), r.raw)
	}
	c.want(c.do("POST", pods+"?dryRun=Some", podBody), 400)
}

func TestDeploymentServiceNamespace(t *testing.T) {
	c := newClient(t)
	dep := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"},"spec":{"replicas":2,"selector":{"matchLabels":{"app":"web"}},
	  "template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"nginx","image":"nginx:1.27"}]}}}}`
	r := c.want(c.do("POST", deploys, dep), 201)
	if r.at("spec", "strategy", "rollingUpdate", "maxSurge") != "25%" || r.at("apiVersion") != "apps/v1" {
		t.Errorf("deployment defaults: %s", r.raw)
	}
	r = c.want(c.do("PUT", deploys+"/web", strings.Replace(dep, `{"app":"web"}},`, `{"app":"web2"}},`, 1)), 422)
	if !strings.Contains(r.raw, "spec.selector") {
		t.Errorf("the selector is immutable: %s", r.raw)
	}
	r = c.want(c.do("PUT", deploys+"/web", strings.Replace(dep, `"replicas":2`, `"replicas":3`, 1)), 200)
	if r.at("metadata", "generation") != float64(2) {
		t.Errorf("a spec change bumps generation: %s", r.raw)
	}

	svcs := "/api/v1/namespaces/default/services"
	r = c.want(c.do("POST", svcs, `{"metadata":{"name":"web"},"spec":{"selector":{"app":"web"},"ports":[{"port":80}]},"status":{"loadBalancer":{}}}`), 201)
	if r.at("spec", "type") != "ClusterIP" || r.at("spec", "ports").([]any)[0].(map[string]any)["targetPort"] != float64(80) {
		t.Errorf("service defaults: %s", r.raw)
	}

	// namespaces: create, refuse delete while not empty, refuse the built-in ones
	nss := "/api/v1/namespaces"
	r = c.want(c.do("POST", nss, `{"metadata":{"name":"team-a"}}`), 201)
	if r.at("status", "phase") != "Active" || r.at("metadata", "namespace") != nil {
		t.Errorf("namespace: %s", r.raw)
	}
	c.want(c.do("POST", "/api/v1/namespaces/team-a/pods", podBody), 201)
	r = c.want(c.do("DELETE", nss+"/team-a", ""), 409)
	if !strings.Contains(r.raw, "CONFLICT retry=never") || !strings.Contains(r.raw, "hint:") {
		t.Errorf("deleting a namespace that is not empty: %s", r.raw)
	}
	c.want(c.do("DELETE", "/api/v1/namespaces/team-a/pods/p1", ""), 200)
	c.want(c.do("DELETE", nss+"/team-a", ""), 200)
	c.want(c.do("DELETE", nss+"/default", ""), 403)
	c.want(c.do("PUT", nss+"/default", `{"metadata":{"name":"default"}}`), 405)
	if r := c.want(c.do("GET", nss, ""), 200); len(r.at("items").([]any)) != 2 {
		t.Errorf("default and kube-system must exist from the start: %s", r.raw)
	}
}

func TestFinalizerHoldsDelete(t *testing.T) {
	c := newClient(t)
	held := strings.Replace(podBody, `"name":"p1"`, `"name":"p1","finalizers":["example.com/hold"]`, 1)
	c.want(c.do("POST", pods, held), 201)
	r := c.want(c.do("DELETE", pods+"/p1", ""), 200)
	if r.at("kind") != "Pod" || r.at("metadata", "deletionTimestamp") == nil {
		t.Fatalf("an object with a finalizer is marked, not removed: %s", r.raw)
	}
	c.want(c.do("GET", pods+"/p1", ""), 200)
	c.want(c.do("PUT", pods+"/p1", podBody), 200) // the finalizer is gone from this body
	c.want(c.do("GET", pods+"/p1", ""), 404)
}
