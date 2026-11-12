package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// Boundaries found in the M0/M1 reviews (docs/meetings/2026-11-11-m1-review.md).

const heldPod = `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"held","finalizers":["example.com/a","example.com/b"]},"spec":{"containers":[{"name":"c","image":"nginx:1.27"}]}}`

// withFinalizers returns the pod as it is stored, with other finalizers, as a PUT body.
func (c *client) withFinalizers(path string, finalizers ...string) string {
	c.t.Helper()
	r := c.want(c.do("GET", path, ""), 200)
	r.body["metadata"].(map[string]any)["finalizers"] = finalizers
	b, _ := json.Marshal(r.body)
	return string(b)
}

func finalizersOf(r reply) string {
	var out []string
	l, _ := r.at("metadata", "finalizers").([]any)
	for _, f := range l {
		out = append(out, f.(string))
	}
	return strings.Join(out, ",")
}

// An object that is being deleted only loses finalizers, by PUT and by every patch format.
func TestNoNewFinalizerWhileDeleting(t *testing.T) {
	c := newClient(t)
	held := pods + "/held"
	c.want(c.do("POST", pods, heldPod), 201)

	// Not being deleted yet: adding one is fine.
	r := c.want(c.patch(held, mergeType, `{"metadata":{"finalizers":["example.com/a","example.com/b","example.com/early"]}}`), 200)
	if finalizersOf(r) != "example.com/a,example.com/b,example.com/early" {
		t.Fatalf("finalizers before the delete: %s", finalizersOf(r))
	}

	r = c.want(c.do("DELETE", held, ""), 200)
	if r.at("metadata", "deletionTimestamp") == nil {
		t.Fatalf("the delete must only mark an object that has finalizers: %s", r.raw)
	}
	rev := c.rev()

	refused := func(name string, r reply) {
		t.Helper()
		c.want(r, 422)
		if r.at("reason") != "Invalid" || !strings.Contains(r.raw, "metadata.finalizers") || !strings.Contains(r.raw, "example.com/new") {
			t.Errorf("%s: the refusal must name the field and the finalizer: %s", name, r.raw)
		}
		if c.rev() != rev {
			t.Fatalf("%s: a refused change advanced the revision", name)
		}
	}
	refused("PUT", c.do("PUT", held, c.withFinalizers(held, "example.com/a", "example.com/b", "example.com/early", "example.com/new")))
	refused("PUT that swaps one for a new one", c.do("PUT", held, c.withFinalizers(held, "example.com/a", "example.com/new")))
	refused("merge patch", c.patch(held, mergeType, `{"metadata":{"finalizers":["example.com/a","example.com/new"]}}`))
	refused("strategic merge patch", c.patch(held, smpType, `{"metadata":{"finalizers":["example.com/new"]}}`))
	refused("JSON patch", c.patch(held, jsonType, `[{"op":"add","path":"/metadata/finalizers/-","value":"example.com/new"}]`))

	// Removing and reordering stay possible, and the last removal completes the delete.
	r = c.want(c.do("PUT", held, c.withFinalizers(held, "example.com/b", "example.com/a")), 200)
	if finalizersOf(r) != "example.com/b,example.com/a" {
		t.Fatalf("after dropping one and reordering: %s", finalizersOf(r))
	}
	r = c.want(c.patch(held, smpType, `{"metadata":{"$deleteFromPrimitiveList/finalizers":["example.com/a"]}}`), 200)
	if finalizersOf(r) != "example.com/b" {
		t.Fatalf("after the strategic removal: %s", finalizersOf(r))
	}
	c.want(c.patch(held, jsonType, `[{"op":"remove","path":"/metadata/finalizers/0"}]`), 200)
	c.want(c.do("GET", held, ""), 404)
}

// A wrong fieldValidation value is refused even when the body has no unknown field.
func TestFieldValidationValueIsAlwaysChecked(t *testing.T) {
	c := newClient(t)
	rev := c.rev()
	for _, bad := range []string{"Maybe", "strict", "true"} {
		r := c.want(c.do("POST", pods+"?fieldValidation="+bad, podBody), 400)
		if !strings.Contains(r.raw, "fieldValidation must be Strict, Warn or Ignore") {
			t.Errorf("POST with fieldValidation=%s: %s", bad, r.raw)
		}
	}
	c.want(c.do("GET", pods+"/p1", ""), 404)
	if c.rev() != rev {
		t.Fatalf("a refused create advanced the revision")
	}

	for _, good := range []string{"Strict", "Warn", "Ignore"} {
		body := strings.Replace(podBody, `"name":"p1"`, `"name":"p-`+strings.ToLower(good)+`"`, 1)
		c.want(c.do("POST", pods+"?fieldValidation="+good, body), 201)
	}
	c.want(c.do("POST", pods, podBody), 201)
	rev = c.rev()

	p1 := pods + "/p1"
	put := c.want(c.do("GET", p1, ""), 200).raw
	c.want(c.do("PUT", p1+"?fieldValidation=Maybe", put), 400)
	c.want(c.do("PUT", p1+"/status?fieldValidation=Maybe", put), 400)
	for _, ct := range []string{mergeType, smpType} {
		c.want(c.patch(p1+"?fieldValidation=Maybe", ct, `{"metadata":{"labels":{"a":"b"}}}`), 400)
	}
	c.want(c.patch(p1+"?fieldValidation=Maybe", jsonType, `[{"op":"add","path":"/metadata/labels/a","value":"b"}]`), 400)
	c.want(c.patch(p1+"/status?fieldValidation=Maybe", mergeType, `{"status":{"phase":"Running"}}`), 400)
	if c.rev() != rev {
		t.Fatalf("a refused update or patch advanced the revision")
	}
	// The same requests with a valid value go through.
	c.want(c.do("PUT", p1+"?fieldValidation=Strict", put), 200)
	c.want(c.patch(p1+"?fieldValidation=Warn", mergeType, `{"metadata":{"labels":{"a":"b"}}}`), 200)
	// dryRun is still checked alongside.
	c.want(c.patch(p1+"?fieldValidation=Strict&dryRun=Some", mergeType, `{}`), 400)
}

// A Deployment's generation moves when its spec or its annotations change, and
// only then. Other kinds keep the plain rule: spec only.
func TestDeploymentGenerationFollowsAnnotations(t *testing.T) {
	c := newClient(t)
	web := deploys + "/web"
	c.want(c.do("POST", deploys, twoDeploy), 201)
	gen := func(r reply) float64 { return r.at("metadata", "generation").(float64) }
	edited := func(change func(meta map[string]any)) string {
		r := c.want(c.do("GET", web, ""), 200)
		change(r.body["metadata"].(map[string]any))
		b, _ := json.Marshal(r.body)
		return string(b)
	}

	r := c.want(c.do("PUT", web, edited(func(m map[string]any) { m["annotations"] = map[string]any{"note": "one"} })), 200)
	if gen(r) != 2 {
		t.Fatalf("PUT that changes only annotations: generation %v, want 2", gen(r))
	}
	rev := c.rev()
	r = c.want(c.do("PUT", web, edited(func(m map[string]any) {})), 200)
	if gen(r) != 2 || c.rev() != rev {
		t.Fatalf("PUT of the same object: generation %v, revision %d -> %d; nothing may move", gen(r), rev, c.rev())
	}
	r = c.want(c.do("PUT", web, edited(func(m map[string]any) { m["labels"] = map[string]any{"app": "web", "tier": "front"} })), 200)
	if gen(r) != 2 {
		t.Fatalf("PUT that changes only labels: generation %v, want 2", gen(r))
	}
	r = c.want(c.patch(web, mergeType, `{"metadata":{"annotations":{"note":"two"}}}`), 200)
	if gen(r) != 3 {
		t.Fatalf("merge patch of an annotation: generation %v, want 3", gen(r))
	}
	r = c.want(c.patch(web, smpType, `{"metadata":{"annotations":{"other":"x"}}}`), 200)
	if gen(r) != 4 {
		t.Fatalf("strategic merge patch that adds an annotation: generation %v, want 4", gen(r))
	}
	r = c.want(c.patch(web, jsonType, `[{"op":"remove","path":"/metadata/annotations/other"}]`), 200)
	if gen(r) != 5 {
		t.Fatalf("JSON patch that removes an annotation: generation %v, want 5", gen(r))
	}
	// Spec and annotations in one write are one step.
	r = c.want(c.patch(web, mergeType, `{"metadata":{"annotations":{"note":"three"}},"spec":{"replicas":7}}`), 200)
	if gen(r) != 6 {
		t.Fatalf("spec and annotations together: generation %v, want 6", gen(r))
	}
	r = c.want(c.patch(web, mergeType, `{"metadata":{"annotations":{"note":"three"}}}`), 200)
	if gen(r) != 6 {
		t.Fatalf("a patch that sets the annotation it already has: generation %v, want 6", gen(r))
	}
	// /status never moves it, whatever the body says about annotations.
	r = c.want(c.do("PUT", web+"/status", edited(func(m map[string]any) { m["annotations"] = map[string]any{"note": "status"} })), 200)
	if gen(r) != 6 || r.at("metadata", "annotations", "note") != "three" {
		t.Fatalf("PUT /status: generation %v, annotations %v", gen(r), r.at("metadata", "annotations"))
	}
	r = c.want(c.patch(web+"/status", mergeType, `{"metadata":{"annotations":{"note":"status"}},"status":{"replicas":1}}`), 200)
	if gen(r) != 6 {
		t.Fatalf("PATCH /status: generation %v, want 6", gen(r))
	}

	// Pods and Services: annotations do not move generation.
	c.want(c.do("POST", pods, podBody), 201)
	if r = c.want(c.patch(pods+"/p1", mergeType, `{"metadata":{"annotations":{"note":"x"}}}`), 200); gen(r) != 1 {
		t.Fatalf("pod annotation: generation %v, want 1", gen(r))
	}
	c.want(c.do("POST", services, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"},"spec":{"ports":[{"port":80}]}}`), 201)
	if r = c.want(c.patch(services+"/s", mergeType, `{"metadata":{"annotations":{"note":"x"}}}`), 200); gen(r) != 1 {
		t.Fatalf("service annotation: generation %v, want 1", gen(r))
	}
}

// A JSON key that appears twice follows fieldValidation, like an unknown field.
func TestDuplicateKeysFollowFieldValidation(t *testing.T) {
	c := newClient(t)
	dup := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"dup"},"spec":{"replicas":1,"replicas":2,"selector":{"matchLabels":{"app":"d"}},
	"template":{"metadata":{"labels":{"app":"d"}},"spec":{"containers":[{"name":"a","image":"a:1","name":"b"}]}}}}`
	warnings := func(r reply) string { return strings.Join(r.header.Values("Warning"), "\n") }

	rev := c.rev()
	r := c.want(c.do("POST", deploys+"?fieldValidation=Strict", dup), 400)
	if !strings.Contains(r.raw, `duplicate field \"spec.replicas\"`) || !strings.Contains(r.raw, `duplicate field \"spec.template.spec.containers[0].name\"`) {
		t.Fatalf("Strict must name every repeated field with its path: %s", r.raw)
	}
	if c.rev() != rev {
		t.Fatalf("a refused create advanced the revision")
	}
	// Unknown and repeated fields are reported together.
	both := strings.Replace(dup, `"metadata":{"name":"dup"}`, `"metadata":{"name":"dup"},"bogus":1`, 1)
	if r = c.want(c.do("POST", deploys+"?fieldValidation=Strict", both), 400); !strings.Contains(r.raw, `unknown field \"bogus\"`) || !strings.Contains(r.raw, "duplicate field") {
		t.Fatalf("unknown and repeated together: %s", r.raw)
	}

	// Warn (also the default): stored with the last value, and the answer says so.
	r = c.want(c.do("POST", deploys, dup), 201)
	if w := warnings(r); !strings.Contains(w, `duplicate field \"spec.replicas\" (the last value is used)`) || len(r.header.Values("Warning")) != 2 {
		t.Fatalf("Warn must answer with one warning per repeated field: %q", w)
	}
	if r.at("spec", "replicas") != float64(2) || names(r.at("spec", "template", "spec", "containers")) != "b" {
		t.Fatalf("the last value must be the one stored: %s", r.raw)
	}
	// Ignore: the same result, silently.
	r = c.want(c.do("POST", deploys+"?fieldValidation=Ignore", strings.Replace(dup, `"name":"dup"`, `"name":"dup2"`, 1)), 201)
	if w := r.header.Values("Warning"); len(w) != 0 || r.at("spec", "replicas") != float64(2) {
		t.Fatalf("Ignore: warnings %v, replicas %v", w, r.at("spec", "replicas"))
	}

	// PUT.
	stored := c.want(c.do("GET", deploys+"/dup", ""), 200).raw
	put := strings.Replace(stored, `"replicas":2`, `"replicas":3,"replicas":4`, 1)
	c.want(c.do("PUT", deploys+"/dup?fieldValidation=Strict", put), 400)
	r = c.want(c.do("PUT", deploys+"/dup", put), 200)
	if !strings.Contains(warnings(r), "spec.replicas") || r.at("spec", "replicas") != float64(4) {
		t.Fatalf("PUT with a repeated key under Warn: %q, replicas %v", warnings(r), r.at("spec", "replicas"))
	}
	// status in a PUT to the main resource is not taken, so it is not judged either.
	stored = c.want(c.do("GET", deploys+"/dup", ""), 200).raw
	withStatus := func(doc string) string { // the stored Deployment has no status member yet
		return strings.TrimSuffix(doc, "}") + `,"status":{"replicas":1,"replicas":2}}`
	}
	c.want(c.do("PUT", deploys+"/dup?fieldValidation=Strict", withStatus(stored)), 200)

	// PATCH: the patch document itself, in all three formats.
	for _, p := range []struct{ ct, body, path string }{
		{mergeType, `{"spec":{"replicas":5,"replicas":6}}`, "spec.replicas"},
		{smpType, `{"spec":{"replicas":5,"replicas":6}}`, "spec.replicas"},
		{jsonType, `[{"op":"replace","path":"/spec/replicas","value":5,"value":6}]`, "[0].value"},
	} {
		if r = c.want(c.patch(deploys+"/dup?fieldValidation=Strict", p.ct, p.body), 400); !strings.Contains(r.raw, "the patch: strict decoding error") || !strings.Contains(r.raw, p.path) {
			t.Errorf("%s Strict: %s", p.ct, r.raw)
		}
		r = c.want(c.patch(deploys+"/dup", p.ct, p.body), 200)
		if !strings.Contains(warnings(r), p.path) || r.at("spec", "replicas") != float64(6) {
			t.Errorf("%s Warn: warnings %q, replicas %v", p.ct, warnings(r), r.at("spec", "replicas"))
		}
		c.want(c.patch(deploys+"/dup", mergeType, `{"spec":{"replicas":4}}`), 200)
	}

	// /status: only what is taken from the body is judged.
	stored = c.want(c.do("GET", deploys+"/dup", ""), 200).raw
	status := withStatus(stored)
	if r = c.want(c.do("PUT", deploys+"/dup/status?fieldValidation=Strict", status), 400); !strings.Contains(r.raw, `duplicate field \"status.replicas\"`) {
		t.Fatalf("PUT /status Strict: %s", r.raw)
	}
	r = c.want(c.do("PUT", deploys+"/dup/status", status), 200)
	if !strings.Contains(warnings(r), "status.replicas") || r.at("status", "replicas") != float64(2) {
		t.Fatalf("PUT /status Warn: %q, %v", warnings(r), r.at("status"))
	}
	// A repeat outside status in a /status write is not taken and not judged.
	stored = c.want(c.do("GET", deploys+"/dup", ""), 200).raw
	c.want(c.do("PUT", deploys+"/dup/status?fieldValidation=Strict", strings.Replace(stored, `"metadata":{`, `"metadata":{"name":"dup","name":"dup",`, 1)), 200)
}
