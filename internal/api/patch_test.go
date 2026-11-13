package api

import (
	"os"
	"strings"
	"testing"
)

const (
	services  = "/api/v1/namespaces/default/services"
	twoDeploy = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","labels":{"app":"web"}},
	"spec":{"replicas":2,"selector":{"matchLabels":{"app":"web"}},"template":{"metadata":{"labels":{"app":"web"}},
	"spec":{"containers":[{"name":"a","image":"a:1","env":[{"name":"X","value":"1"},{"name":"Y","value":"2"}]},{"name":"b","image":"b:1"}]}}}}`
	smpType   = "application/strategic-merge-patch+json"
	mergeType = "application/merge-patch+json"
	jsonType  = "application/json-patch+json"
)

func (c *client) patch(path, contentType, body string) reply {
	c.t.Helper()
	return c.do("PATCH", path, body, "Content-Type", contentType)
}

func names(v any) string {
	var out []string
	l, _ := v.([]any)
	for _, e := range l {
		out = append(out, e.(map[string]any)["name"].(string))
	}
	return strings.Join(out, ",")
}

func TestPatchMerge(t *testing.T) {
	c := newClient(t)
	c.want(c.do("POST", pods, podBody), 201)
	before := c.rev()
	r := c.want(c.patch(pods+"/p1", mergeType, `{"metadata":{"labels":{"tier":"front","app":null}}}`), 200)
	if l := r.at("metadata", "labels").(map[string]any); l["tier"] != "front" || len(l) != 1 {
		t.Fatalf("labels after merge patch: %v", l)
	}
	if r.at("metadata", "generation") != float64(1) {
		t.Fatalf("a label change must not move generation: %v", r.at("metadata", "generation"))
	}
	if r.at("status", "phase") != "Pending" {
		t.Fatalf("status was lost: %v", r.at("status"))
	}
	if c.rev() != before+1 {
		t.Fatalf("one patch must be one revision: %d -> %d", before, c.rev())
	}
	// The same patch again changes nothing and writes nothing.
	c.want(c.patch(pods+"/p1", mergeType, `{"metadata":{"labels":{"tier":"front"}}}`), 200)
	if c.rev() != before+1 {
		t.Fatalf("a patch that changes nothing advanced the revision")
	}
}

func TestPatchStrategic(t *testing.T) {
	c := newClient(t)
	c.want(c.do("POST", deploys, twoDeploy), 201)
	// What kubectl apply sends when a container is added, one removed, the order changed and an env removed.
	r := c.want(c.patch(deploys+"/web", smpType, `{"spec":{"template":{"spec":{
		"$setElementOrder/containers":[{"name":"c"},{"name":"a"}],
		"containers":[{"name":"c","image":"c:1"},{"name":"a","image":"a:2","$setElementOrder/env":[{"name":"Y"}],"env":[{"name":"X","$patch":"delete"}]},{"name":"b","$patch":"delete"}]}}}}`), 200)
	cs := r.at("spec", "template", "spec", "containers")
	if names(cs) != "c,a" {
		t.Fatalf("containers after the patch: %s", names(cs))
	}
	a := cs.([]any)[1].(map[string]any)
	if a["image"] != "a:2" || names(a["env"]) != "Y" {
		t.Fatalf("container a after the patch: %v", a)
	}
	if got := cs.([]any)[0].(map[string]any)["imagePullPolicy"]; got != "IfNotPresent" {
		t.Fatalf("the added container was not defaulted: %v", got)
	}
	if r.at("metadata", "generation") != float64(2) {
		t.Fatalf("a template change must move generation to 2: %v", r.at("metadata", "generation"))
	}

	// retainKeys: switching the strategy type drops rollingUpdate.
	r = c.want(c.patch(deploys+"/web", smpType, `{"spec":{"strategy":{"$retainKeys":["type"],"type":"Recreate"}}}`), 200)
	if s := r.at("spec", "strategy").(map[string]any); s["type"] != "Recreate" || len(s) != 1 {
		t.Fatalf("strategy after retainKeys: %v", s)
	}
	// Without it the leftover rollingUpdate is invalid for Recreate, and the server says so.
	c.want(c.patch(deploys+"/web", smpType, `{"spec":{"strategy":{"$retainKeys":["type","rollingUpdate"],"type":"RollingUpdate","rollingUpdate":{"maxSurge":1}}}}`), 200)
	r = c.want(c.patch(deploys+"/web", smpType, `{"spec":{"strategy":{"type":"Recreate"}}}`), 422)
	if !strings.Contains(r.raw, "spec.strategy.rollingUpdate") {
		t.Fatalf("expected the rollingUpdate complaint: %s", r.raw)
	}

	for _, bad := range []struct{ name, body, want string }{
		{"unknown directive", `{"spec":{"$frob/replicas":3}}`, "unknown directive"},
		{"unknown $patch value", `{"spec":{"strategy":{"$patch":"merge"}}}`, "$patch"},
		{"element without its merge key", `{"spec":{"template":{"spec":{"containers":[{"image":"x"}]}}}}`, "merge key"},
		{"selector is immutable", `{"spec":{"selector":{"matchLabels":{"app":"other"}},"template":{"metadata":{"labels":{"app":"other"}}}}}`, "spec.selector"},
	} {
		if r := c.want(c.patch(deploys+"/web", smpType, bad.body), 422); !strings.Contains(r.raw, bad.want) {
			t.Errorf("%s: %s", bad.name, r.raw)
		}
	}
}

func TestPatchJSON(t *testing.T) {
	c := newClient(t)
	c.want(c.do("POST", deploys, twoDeploy), 201)
	r := c.want(c.patch(deploys+"/web", jsonType, `[{"op":"test","path":"/spec/replicas","value":2},{"op":"replace","path":"/spec/replicas","value":5},{"op":"remove","path":"/spec/template/spec/containers/1"}]`), 200)
	if r.at("spec", "replicas") != float64(5) || names(r.at("spec", "template", "spec", "containers")) != "a" {
		t.Fatalf("after the JSON patch: replicas %v, containers %s", r.at("spec", "replicas"), names(r.at("spec", "template", "spec", "containers")))
	}
	before := c.rev()
	r = c.want(c.patch(deploys+"/web", jsonType, `[{"op":"test","path":"/spec/replicas","value":2},{"op":"replace","path":"/spec/replicas","value":9}]`), 422)
	if !strings.Contains(r.raw, "operation 0 (test)") || c.rev() != before {
		t.Fatalf("a failed test operation must stop the patch and write nothing: %s", r.raw)
	}
}

func TestPatchRequestRules(t *testing.T) {
	c := newClient(t)
	c.want(c.do("POST", pods, podBody), 201)
	rev := c.rev()

	c.want(c.patch(pods+"/nope", mergeType, `{}`), 404)
	if r := c.want(c.patch(pods+"/p1", "application/apply-patch+yaml", `{}`), 415); !strings.Contains(r.raw, "server-side apply") {
		t.Fatalf("server-side apply must be refused with a reason: %s", r.raw)
	}
	c.want(c.patch(pods+"/p1", "text/plain", `{}`), 415)
	c.want(c.patch(pods+"/p1", mergeType, `[1]`), 400)
	c.want(c.patch(pods+"/p1", mergeType, `{not json`), 400)

	// A resourceVersion in the patch is a precondition.
	if r := c.want(c.patch(pods+"/p1", mergeType, `{"metadata":{"resourceVersion":"1","labels":{"a":"b"}}}`), 409); r.at("reason") != "Conflict" {
		t.Fatalf("stale resourceVersion in a patch: %s", r.raw)
	}
	// Server-owned metadata does not move.
	r := c.want(c.patch(pods+"/p1", mergeType, `{"metadata":{"creationTimestamp":"2001-01-01T00:00:00Z","generation":99}}`), 200)
	if r.at("metadata", "generation") != float64(1) || strings.HasPrefix(r.at("metadata", "creationTimestamp").(string), "2001") {
		t.Fatalf("server-owned metadata was patched: %v", r.at("metadata"))
	}
	// A pod's spec stays immutable through a patch too.
	c.want(c.patch(pods+"/p1", smpType, `{"spec":{"restartPolicy":"Never"}}`), 422)
	c.want(c.patch(pods+"/p1", smpType, `{"spec":{"containers":[{"name":"c","image":"nginx:1.28"}]}}`), 200)

	// Unknown fields follow fieldValidation.
	if r := c.want(c.patch(pods+"/p1?fieldValidation=Strict", mergeType, `{"spec":{"nodeNam":"n1"}}`), 400); !strings.Contains(r.raw, `unknown field \"spec.nodeNam\"`) {
		t.Fatalf("strict unknown field: %s", r.raw)
	}
	r = c.want(c.patch(pods+"/p1", mergeType, `{"spec":{"nodeNam":"n1"}}`), 200)
	if w := r.header.Values("Warning"); len(w) != 1 || !strings.Contains(w[0], "spec.nodeNam") {
		t.Fatalf("an unknown field in a patch must warn: %v", w)
	}

	// Dry run: the answer shows the result, nothing is stored.
	rev = c.rev()
	r = c.want(c.patch(pods+"/p1?dryRun=All", mergeType, `{"metadata":{"labels":{"dry":"run"}}}`), 200)
	if r.at("metadata", "labels", "dry") != "run" || c.rev() != rev {
		t.Fatalf("dry-run patch: labels %v, revision %d -> %d", r.at("metadata", "labels"), rev, c.rev())
	}
}

func TestPatchStatus(t *testing.T) {
	c := newClient(t)
	c.want(c.do("POST", pods, podBody), 201)
	r := c.want(c.patch(pods+"/p1/status", mergeType, `{"status":{"phase":"Running","podIP":"10.1.0.5"},"metadata":{"labels":{"ignored":"yes"}}}`), 200)
	if r.at("status", "phase") != "Running" || r.at("metadata", "labels", "ignored") != nil || r.at("metadata", "generation") != float64(1) {
		t.Fatalf("a status patch takes status only: %s", r.raw)
	}
	// conditions merge by type.
	c.want(c.patch(pods+"/p1/status", smpType, `{"status":{"conditions":[{"type":"Ready","status":"False"}]}}`), 200)
	r = c.want(c.patch(pods+"/p1/status", smpType, `{"status":{"conditions":[{"type":"PodScheduled","status":"True"},{"type":"Ready","status":"True"}]}}`), 200)
	conds := r.at("status", "conditions").([]any)
	if len(conds) != 2 || r.at("status", "phase") != "Running" {
		t.Fatalf("conditions after two status patches: %v", conds)
	}
	// The main resource ignores status in a patch.
	r = c.want(c.patch(pods+"/p1", mergeType, `{"status":{"phase":"Failed"}}`), 200)
	if r.at("status", "phase") != "Running" {
		t.Fatalf("status changed through the main resource: %v", r.at("status", "phase"))
	}
}

func (c *client) protobuf(path, file string) reply {
	c.t.Helper()
	body, err := os.ReadFile("../protobuf/testdata/kubectl-v1.36.6/" + file + ".bin")
	if err != nil {
		c.t.Fatal(err)
	}
	return c.do("POST", path, string(body), "Content-Type", "application/vnd.kubernetes.protobuf", "Accept", "application/vnd.kubernetes.protobuf,application/json")
}

// The bodies kubectl really sends for `create <kind>` (and `debug --copy-to`).
func TestCreateFromProtobuf(t *testing.T) {
	c := newClient(t)
	r := c.want(c.protobuf(deploys, "create-deployment"), 201)
	if r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("the answer must be JSON, got %q", r.header.Get("Content-Type"))
	}
	if r.at("spec", "replicas") != float64(1) || r.at("spec", "strategy", "type") != "RollingUpdate" || r.at("metadata", "labels", "app") != "web" {
		t.Fatalf("create deployment: %s", r.raw)
	}
	if w := r.header.Values("Warning"); len(w) != 0 {
		t.Fatalf("a plain `kubectl create deployment` must not warn: %v", w)
	}
	r = c.want(c.protobuf(deploys, "create-deployment-zero-replicas"), 201)
	if r.at("spec", "replicas") != float64(0) {
		t.Fatalf("--replicas=0 became %v", r.at("spec", "replicas"))
	}
	r = c.want(c.protobuf(deploys, "create-deployment-command"), 201)
	if cmd := r.at("spec", "template", "spec", "containers").([]any)[0].(map[string]any)["command"].([]any); len(cmd) != 2 || cmd[0] != "sleep" {
		t.Fatalf("command: %v", cmd)
	}
	c.want(c.protobuf(deploys, "create-deployment"), 409)

	r = c.want(c.protobuf(services, "create-service-clusterip-two-ports"), 201)
	if p := r.at("spec", "ports").([]any); len(p) != 2 || p[1].(map[string]any)["targetPort"] != "https" || p[0].(map[string]any)["protocol"] != "TCP" {
		t.Fatalf("service ports: %v", p)
	}
	r = c.want(c.protobuf(services, "create-service-clusterip-headless"), 201)
	if w := r.header.Values("Warning"); len(w) != 1 || !strings.Contains(w[0], "spec.clusterIP") {
		t.Fatalf("a headless service must carry the stored-only warning: %v", w)
	}
	if r := c.want(c.protobuf(services, "create-service-nodeport"), 422); !strings.Contains(r.raw, "Not supported by Shearwater") {
		t.Fatalf("NodePort must be refused with the reason: %s", r.raw)
	}
	r = c.want(c.protobuf("/api/v1/namespaces", "create-namespace"), 201)
	if r.at("status", "phase") != "Active" || r.at("metadata", "name") != "team-b" {
		t.Fatalf("create namespace: %s", r.raw)
	}
	r = c.want(c.protobuf(pods, "debug-copy-pod"), 201)
	if r.at("metadata", "name") != "solo-copy" || r.at("spec", "volumes").([]any)[0].(map[string]any)["emptyDir"] == nil {
		t.Fatalf("pod copy: %s", r.raw)
	}

	// The body's kind must be the endpoint's.
	c.want(c.protobuf(pods, "create-deployment"), 400)
	// Damaged bodies are refused with the way out.
	r = c.want(c.do("POST", deploys, "k8s\x00garbage", "Content-Type", "application/vnd.kubernetes.protobuf"), 400)
	if !strings.Contains(r.raw, "kubectl apply -f") {
		t.Fatalf("a protobuf error must point to manifests: %s", r.raw)
	}
}

func TestStoredOnlyWarnings(t *testing.T) {
	c := newClient(t)
	body := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"probed"},"spec":{"dnsPolicy":"ClusterFirst","containers":[{"name":"c","image":"nginx:1.27",
		"livenessProbe":{"httpGet":{"path":"/","port":80}},"resources":{"requests":{"cpu":"100m"}}}]}}`
	r := c.want(c.do("POST", pods, body), 201)
	w := strings.Join(r.header.Values("Warning"), "\n")
	if strings.Contains(w, "livenessProbe") || !strings.Contains(w, "spec.containers[0].resources.requests") {
		t.Fatalf("stored-only fields must warn on create: %q", w)
	}
	if strings.Contains(w, "dnsPolicy") {
		t.Fatalf("dnsPolicy: ClusterFirst is what kubectl run always sends and must not warn: %q", w)
	}
	if r.at("spec", "containers").([]any)[0].(map[string]any)["livenessProbe"] == nil {
		t.Fatalf("the supported probe was not stored")
	}
	// A later change that does not touch them stays quiet; one that adds another warns about that one only.
	r = c.want(c.patch(pods+"/probed", mergeType, `{"metadata":{"labels":{"a":"b"}}}`), 200)
	if w := r.header.Values("Warning"); len(w) != 0 {
		t.Fatalf("an unrelated patch repeated the warnings: %v", w)
	}
	c.want(c.do("POST", deploys, twoDeploy), 201)
	r = c.want(c.patch(deploys+"/web", smpType, `{"spec":{"minReadySeconds":5,"template":{"spec":{"nodeSelector":{"disk":"ssd"}}}}}`), 200)
	if w := r.header.Values("Warning"); len(w) != 2 {
		t.Fatalf("expected warnings for minReadySeconds and nodeSelector: %v", w)
	}
}
