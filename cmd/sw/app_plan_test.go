package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAppIntentCreateAndPreserveUnrelatedFields(t *testing.T) {
	c, root, st := planTestServer(t)
	ctx := context.Background()
	before, _ := st.Rev()
	p, err := prepareAppPlan(ctx, c, root, "127.0.0.1:6443", appIntent{Action: "create", Name: "web", Image: "nginx:alpine", Port: 80})
	if err != nil || len(p.Actions) != 2 {
		t.Fatal(p, err)
	}
	after, _ := st.Rev()
	if before != after {
		t.Fatal("planning mutated state")
	}
	for _, a := range p.Actions {
		if a.Method != "POST" {
			t.Fatal("adoption allowed")
		}
	}
	// Add configuration that an image-only update must keep, then create the
	// Deployment through the real API. No runtime/containers are involved.
	v, _ := objectMap(p.Actions[0].Body)
	spec := v["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	podSpec := template["spec"].(map[string]any)
	containers := podSpec["containers"].([]any)
	app := containers[0].(map[string]any)
	app["env"] = []any{map[string]any{"name": "KEEP", "value": "sentinel"}}
	podSpec["volumes"] = []any{map[string]any{"name": "cache", "emptyDir": map[string]any{}}}
	containers = append(containers, map[string]any{"name": "sidecar", "image": "busybox:1.37"})
	podSpec["containers"] = containers
	b, _ := json.Marshal(v)
	created, _, _, err := apiRequest(ctx, c, "POST", "/apis/apps/v1/namespaces/default/deployments", b)
	if err != nil {
		t.Fatal(err)
	}
	obj, _ := objectMap(created)
	meta := objectMeta(obj)
	i := appIntent{Action: "image", Name: "web", Image: "nginx:stable", Container: "app", UID: meta["uid"].(string), Revision: meta["resourceVersion"].(string)}
	p, err = prepareAppPlan(ctx, c, root, "127.0.0.1:6443", i)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(p.Actions[0].Body, []byte("sentinel")) || !bytes.Contains(p.Actions[0].Body, []byte("busybox:1.37")) || !bytes.Contains(p.Actions[0].Body, []byte("emptyDir")) || !bytes.Contains(p.Actions[0].Body, []byte("tcpSocket")) {
		t.Fatal("unrelated settings lost")
	}
	i.Container = ""
	if _, err = prepareAppPlan(ctx, c, root, "127.0.0.1:6443", i); err == nil {
		t.Fatal("ambiguous container accepted")
	}
	i.Container = "app"
	i.Revision = "0"
	if _, err = prepareAppPlan(ctx, c, root, "127.0.0.1:6443", i); err == nil || !strings.Contains(err.Error(), "CONFLICT") {
		t.Fatal(err)
	}
	if _, err = prepareAppPlan(ctx, c, root, "127.0.0.1:6443", appIntent{Action: "create", Name: "web", Image: "nginx", Port: 80}); err == nil {
		t.Fatal("create adopted existing deployment")
	}
}

func TestAppIntentScaleAndInvalidActions(t *testing.T) {
	c, root, _ := planTestServer(t)
	ctx := context.Background()
	for _, i := range []appIntent{{Action: "delete", Name: "web"}, {Action: "create", Name: "web", Image: "nginx", Port: 0}, {Action: "image", Name: "web", Image: "nginx"}, {Action: "scale", Name: "web", UID: "a", Revision: "1"}} {
		if _, err := prepareAppPlan(ctx, c, root, "127.0.0.1:6443", i); err == nil {
			t.Fatal("invalid intent accepted", i)
		}
	}
	p, err := prepareAppPlan(ctx, c, root, "127.0.0.1:6443", appIntent{Action: "create", Name: "web", Image: "nginx", Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	b, _, _, err := apiRequest(ctx, c, "POST", "/apis/apps/v1/namespaces/default/deployments", p.Actions[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := objectMap(b)
	m := objectMeta(v)
	zero := int32(0)
	p, err = prepareAppPlan(ctx, c, root, "127.0.0.1:6443", appIntent{Action: "scale", Name: "web", Replicas: &zero, UID: m["uid"].(string), Revision: m["resourceVersion"].(string)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(p.Actions[0].Body, []byte(`"replicas":0`)) {
		t.Fatal("scale to zero lost")
	}
}
