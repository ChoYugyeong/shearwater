package api

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const canonicalScaleSelector = "a in (blue,green),app=web,b notin (test,unused),!c,d,z=live"

func createScaleWorkload(c *client, collection, kind string) string {
	c.t.Helper()
	body := fmt.Sprintf(`{"apiVersion":"apps/v1","kind":%q,
		"metadata":{"name":"web","labels":{"team":"infra"},"annotations":{"note":"keep"}},
		"spec":{"replicas":2,"selector":{"matchLabels":{"z":"live","app":"web"},"matchExpressions":[
			{"key":"d","operator":"Exists"},{"key":"c","operator":"DoesNotExist"},
			{"key":"b","operator":"NotIn","values":["unused","test"]},
			{"key":"a","operator":"In","values":["green","blue"]}]},
			"template":{"metadata":{"labels":{"z":"live","app":"web","a":"blue","d":""}},
			"spec":{"containers":[{"name":"web","image":"nginx:1.27"}]}}}}`, kind)
	c.want(c.do("POST", collection+"?fieldValidation=Strict", body), 201)
	return collection + "/web"
}

func TestScaleDiscoveryAndRoutes(t *testing.T) {
	c := newClient(t)
	resources := c.want(c.do("GET", "/apis/apps/v1", ""), 200).at("resources").([]any)
	var scales []string
	for _, resource := range resources {
		r := resource.(map[string]any)
		name := r["name"].(string)
		if !strings.HasSuffix(name, "/scale") {
			continue
		}
		if r["group"] != "autoscaling" || r["version"] != "v1" || r["kind"] != "Scale" || r["namespaced"] != true ||
			!reflect.DeepEqual(r["verbs"], []any{"get", "patch", "update"}) {
			t.Errorf("scale discovery for %s: %v", name, r)
		}
		scales = append(scales, name)
	}
	slices.Sort(scales)
	if !slices.Equal(scales, []string{"deployments/scale", "replicasets/scale"}) {
		t.Errorf("scale discovery entries: %v", scales)
	}
	c.want(c.do("GET", pods+"/web/scale", ""), 404)
	c.want(c.do("GET", "/apis/apps/v1/deployments/web/scale", ""), 404)
	c.want(c.do("GET", deploys+"//scale", ""), 404)
	c.want(c.do("GET", "/apis/autoscaling/v1/namespaces/default/scales/web", ""), 404)
	c.want(c.do("POST", deploys+"/web/scale", `{}`), 405)
	c.want(c.do("DELETE", deploys+"/web/scale", ""), 405)
	c.want(c.do("GET", deploys+"/missing/scale", ""), 404)
	c.want(c.do("PUT", deploys+"/missing/scale", `{"spec":{"replicas":1}}`), 404)
	c.want(c.do("PATCH", deploys+"/missing/scale", `{}`, "Content-Type", mergePatchType), 404)
}

func TestScaleProjectionAndWrites(t *testing.T) {
	for _, parent := range []struct{ kind, collection string }{
		{"Deployment", deploys},
		{"ReplicaSet", "/apis/apps/v1/namespaces/default/replicasets"},
	} {
		t.Run(parent.kind, func(t *testing.T) {
			c := newClient(t)
			path := createScaleWorkload(c, parent.collection, parent.kind)
			c.want(c.do("PUT", path+"/status", `{"status":{"replicas":3,"readyReplicas":1}}`), 200)
			before := c.want(c.do("GET", path, ""), 200)
			uid := before.at("metadata", "uid")
			scale := c.want(c.do("GET", path+"/scale", ""), 200)
			wantMeta := map[string]any{"name": "web", "namespace": "default", "uid": uid,
				"resourceVersion": before.at("metadata", "resourceVersion"), "creationTimestamp": before.at("metadata", "creationTimestamp")}
			wantStatus := map[string]any{"replicas": float64(3), "selector": canonicalScaleSelector}
			if scale.at("apiVersion") != "autoscaling/v1" || scale.at("kind") != "Scale" ||
				scale.at("spec", "replicas") != float64(2) || !reflect.DeepEqual(scale.at("metadata"), wantMeta) ||
				!reflect.DeepEqual(scale.at("status"), wantStatus) {
				t.Fatalf("unexpected Scale projection: %s", scale.raw)
			}
			for _, step := range []struct {
				method, contentType, body string
				replicas                  int
			}{
				{"PATCH", mergePatchType, `{"spec":{"replicas":0},"status":{"replicas":99,"selector":"other"},"metadata":{"annotations":{"note":"changed"},"finalizers":["example.com/hold"]}}`, 0},
				{"PATCH", jsonPatchType, fmt.Sprintf(`[{"op":"test","path":"/status/selector","value":%q},{"op":"replace","path":"/spec/replicas","value":4}]`, canonicalScaleSelector), 4},
				{"PATCH", strategicPatchType, `{"spec":{"$patch":"replace","replicas":5},"metadata":{"labels":{"team":"other"}}}`, 5},
				{"PUT", "application/json", `{"apiVersion":"autoscaling/v1","kind":"Scale","metadata":{"generation":99},"spec":{"replicas":6},"status":"ignored"}`, 6},
			} {
				rev := c.rev()
				r := c.want(c.do(step.method, path+"/scale?fieldValidation=Strict", step.body, "Content-Type", step.contentType), 200)
				wantMeta["resourceVersion"] = strconv.FormatInt(rev+1, 10)
				if r.at("kind") != "Scale" || r.at("spec", "replicas") != float64(step.replicas) ||
					!reflect.DeepEqual(r.at("status"), wantStatus) || !reflect.DeepEqual(r.at("metadata"), wantMeta) {
					t.Errorf("%s %s response: %s", step.method, step.contentType, r.raw)
				}
				m := before.at("metadata").(map[string]any)
				m["resourceVersion"] = wantMeta["resourceVersion"]
				m["generation"] = m["generation"].(float64) + 1
				before.at("spec").(map[string]any)["replicas"] = float64(step.replicas)
				after := c.want(c.do("GET", path, ""), 200)
				if !reflect.DeepEqual(after.body, before.body) || c.rev() != rev+1 {
					t.Fatalf("%s %s changed unexpected parent fields or revisions: %s", step.method, step.contentType, after.raw)
				}
				before = after
			}
		})
	}
}

func TestScaleDryRunAndNoop(t *testing.T) {
	c := newClient(t)
	path := createScaleWorkload(c, deploys, "Deployment")
	scalePath := path + "/scale"
	before := c.want(c.do("GET", path, ""), 200)
	rev := c.rev()
	for _, method := range []string{"PUT", "PATCH"} {
		ct := "application/json"
		if method == "PATCH" {
			ct = mergePatchType
		}
		dry := c.want(c.do(method, scalePath+"?dryRun=All", `{"spec":{"replicas":9}}`, "Content-Type", ct), 200)
		if dry.at("spec", "replicas") != float64(9) {
			t.Errorf("%s dry-run projection: %s", method, dry.raw)
		}
		noop := c.want(c.do(method, scalePath, `{"spec":{"replicas":2},"metadata":{"annotations":{"different":"value"}}}`, "Content-Type", ct), 200)
		if noop.at("metadata", "resourceVersion") != before.at("metadata", "resourceVersion") {
			t.Errorf("%s no-op projection: %s", method, noop.raw)
		}
		if after := c.want(c.do("GET", path, ""), 200); after.raw != before.raw || c.rev() != rev {
			t.Fatalf("%s dry-run or no-op changed the parent: %s", method, after.raw)
		}
	}
}

func TestScaleValidation(t *testing.T) {
	c := newClient(t)
	path := createScaleWorkload(c, deploys, "Deployment") + "/scale"
	rev := c.rev()
	for _, tc := range []struct {
		method, contentType, query, body string
		code                             int
	}{
		{"PUT", "application/json", "", `{"spec":{"replicas":-1}}`, 422},
		{"PUT", "application/json", "", `{"spec":{"replicas":2147483648}}`, 400},
		{"PUT", "application/json", "", `{"spec":{"replicas":"2"}}`, 400},
		{"PUT", "application/json", "", `{"kind":"Deployment","spec":{"replicas":2}}`, 400},
		{"PUT", "application/json", "", `{"metadata":{"name":"other"},"spec":{"replicas":2}}`, 400},
		{"PUT", "application/json", "", `{"metadata":{"namespace":"other"},"spec":{"replicas":2}}`, 400},
		{"PUT", "application/json", "?fieldValidation=Strict", `{"spec":{"replicas":2,"replicas":3}}`, 400},
		{"PUT", "application/json", "?dryRun=invalid", `{"spec":{"replicas":3}}`, 400},
		{"PATCH", mergePatchType, "?fieldValidation=invalid", `{}`, 400},
		{"PATCH", mergePatchType, "?fieldValidation=Strict", `{"spec":{"template":{},"replicas":3}}`, 400},
		{"PATCH", strategicPatchType, "?fieldValidation=Strict", `{"spec":{"replicas":2,"replicas":3}}`, 400},
		{"PATCH", jsonPatchType, "?fieldValidation=Strict", `[{"op":"add","op":"replace","path":"/spec/replicas","value":3}]`, 400},
		{"PATCH", jsonPatchType, "", `[{"op":"test","path":"/spec/replicas","value":1},{"op":"replace","path":"/spec/replicas","value":3}]`, 422},
		{"PATCH", applyPatchType, "", `spec: { replicas: 3 }`, 415},
	} {
		r := c.want(c.do(tc.method, path+tc.query, tc.body, "Content-Type", tc.contentType), tc.code)
		if r.at("kind") != "Status" {
			t.Errorf("expected a Status for %s %s: %s", tc.method, tc.body, r.raw)
		}
	}
	for _, mode := range []string{"Warn", "Ignore"} {
		r := c.want(c.do("PATCH", path+"?dryRun=All&fieldValidation="+mode, `{"spec":{"replicas":2,"replicas":4,"extra":true}}`, "Content-Type", mergePatchType), 200)
		wantWarnings := 0
		if mode == "Warn" {
			wantWarnings = 2
		}
		if r.at("spec", "replicas") != float64(4) || len(r.header.Values("Warning")) != wantWarnings {
			t.Errorf("%s fieldValidation: %s %v", mode, r.raw, r.header.Values("Warning"))
		}
	}
	if c.rev() != rev {
		t.Fatal("rejected or dry-run Scale writes advanced the revision")
	}
}

func TestScalePreconditions(t *testing.T) {
	c := newClient(t)
	path := createScaleWorkload(c, deploys, "Deployment")
	scalePath := path + "/scale"
	cur := c.want(c.do("GET", scalePath, ""), 200)
	rv, uid := cur.at("metadata", "resourceVersion"), cur.at("metadata", "uid")
	rev := c.rev()
	start := make(chan struct{})
	replies := make(chan reply, 2)
	go func() {
		<-start
		replies <- c.do("PUT", scalePath, fmt.Sprintf(`{"metadata":{"resourceVersion":%q,"uid":%q},"spec":{"replicas":3}}`, rv, uid))
	}()
	go func() {
		<-start
		replies <- c.do("PATCH", scalePath, fmt.Sprintf(`{"metadata":{"resourceVersion":%q,"uid":%q},"spec":{"replicas":4}}`, rv, uid), "Content-Type", mergePatchType)
	}()
	close(start)
	first, second := <-replies, <-replies
	if first.code == 409 {
		first, second = second, first
	}
	c.want(first, 200)
	c.want(second, 409)
	if second.at("reason") != "Conflict" || c.rev() != rev+1 {
		t.Fatalf("expected one committed Scale update and one conflict: %s %s", first.raw, second.raw)
	}
	// A stale precondition fails even for a request that would change nothing.
	c.want(c.do("PUT", scalePath, fmt.Sprintf(`{"metadata":{"resourceVersion":%q},"spec":{"replicas":%.0f}}`, rv, first.at("spec", "replicas"))), 409)
	c.want(c.do("DELETE", path, ""), 200)
	createScaleWorkload(c, deploys, "Deployment")
	before := c.want(c.do("GET", path, ""), 200)
	rev = c.rev()
	c.want(c.do("PATCH", scalePath, fmt.Sprintf(`{"metadata":{"uid":%q},"spec":{"replicas":9}}`, uid), "Content-Type", mergePatchType), 409)
	c.want(c.do("PUT", scalePath+"?dryRun=All", fmt.Sprintf(`{"metadata":{"uid":%q},"spec":{"replicas":9}}`, uid)), 409)
	if after := c.want(c.do("GET", path, ""), 200); after.raw != before.raw || c.rev() != rev {
		t.Fatalf("stale UID changed the replacement Deployment: %s", after.raw)
	}
}
