package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"shearwater/internal/apis"
)

// AppIntent covers deliberately small operations. Existing Deployments are read
// and changed in place, so changing an image does not remove env, volumes, probes
// or sidecars. This is not an image builder or a Compose/Kubernetes replacement.
type appIntent struct {
	Action    string `json:"action"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Image     string `json:"image,omitempty"`
	Container string `json:"container,omitempty"`
	Replicas  *int32 `json:"replicas,omitempty"`
	Port      int32  `json:"port,omitempty"`
	UID       string `json:"uid,omitempty"`
	Revision  string `json:"resource_version,omitempty"`
}

func prepareAppPlan(ctx context.Context, c *agentClient, config, listen string, i appIntent) (deploymentPlan, error) {
	fail := func(message string) (deploymentPlan, error) {
		return deploymentPlan{}, fmt.Errorf("invalid app intent: %s", message)
	}
	if i.Namespace == "" {
		i.Namespace = "default"
	}
	if !apis.IsDNS1123Label(i.Namespace) || !apis.IsDNS1123Label(i.Name) {
		return fail("use a DNS label for name and namespace")
	}
	if i.Replicas != nil && (*i.Replicas < 0 || *i.Replicas > 1000) {
		return fail("replicas must be 0..1000")
	}
	var manifest any
	switch i.Action {
	case "create":
		if i.Image == "" || i.Port < 1 || i.Port > 65535 || i.Container != "" || i.UID != "" || i.Revision != "" {
			return fail("create requires image and port 1..65535; no existing identity or container")
		}
		// Never adopt a same-named Deployment or Service. The saved POST actions
		// still recheck absence on apply, including a race after this read.
		for _, path := range []string{"/apis/apps/v1/namespaces/" + i.Namespace + "/deployments/" + i.Name, "/api/v1/namespaces/" + i.Namespace + "/services/" + i.Name} {
			_, code, _, err := apiRequest(ctx, c, "GET", path, nil)
			if code != 404 {
				if err != nil {
					return deploymentPlan{}, err
				}
				return fail("target name already exists; use image or scale on an observed Deployment")
			}
		}
		n := int32(1)
		if i.Replicas != nil {
			n = *i.Replicas
		}
		labels := map[string]any{"app.kubernetes.io/name": i.Name}
		meta := map[string]any{"name": i.Name, "namespace": i.Namespace}
		container := map[string]any{"name": "app", "image": i.Image, "ports": []any{map[string]any{"containerPort": i.Port}}, "readinessProbe": map[string]any{"tcpSocket": map[string]any{"port": i.Port}}}
		manifest = map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{
			map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta, "spec": map[string]any{"replicas": n, "selector": map[string]any{"matchLabels": labels}, "template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": map[string]any{"containers": []any{container}}}}},
			map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": meta, "spec": map[string]any{"selector": labels, "ports": []any{map[string]any{"port": i.Port, "targetPort": i.Port, "protocol": "TCP"}}}},
		}}
	case "image", "scale":
		if i.UID == "" || i.Revision == "" || i.Port != 0 {
			return fail("updates require observed uid and resource_version; port changes are not supported")
		}
		if i.Action == "image" && (i.Image == "" || i.Replicas != nil) || i.Action == "scale" && (i.Replicas == nil || i.Image != "" || i.Container != "") {
			return fail("image changes only image; scale changes only replicas")
		}
		b, _, _, err := apiRequest(ctx, c, "GET", "/apis/apps/v1/namespaces/"+i.Namespace+"/deployments/"+i.Name, nil)
		if err != nil {
			return deploymentPlan{}, err
		}
		v, err := objectMap(b)
		if err != nil {
			return deploymentPlan{}, err
		}
		m := objectMeta(v)
		if m["uid"] != i.UID || m["resourceVersion"] != i.Revision {
			return fail("CONFLICT: observation is stale; observe again")
		}
		spec, _ := v["spec"].(map[string]any)
		if spec == nil {
			return fail("Deployment spec missing")
		}
		if i.Action == "scale" {
			spec["replicas"] = *i.Replicas
		} else {
			template, _ := spec["template"].(map[string]any)
			podSpec, _ := template["spec"].(map[string]any)
			containers, _ := podSpec["containers"].([]any)
			if len(containers) > 1 && i.Container == "" {
				return fail("choose container for a multi-container Deployment")
			}
			found := false
			for _, raw := range containers {
				container, _ := raw.(map[string]any)
				if container == nil {
					return fail("invalid container object returned by server")
				}
				if len(containers) == 1 && i.Container == "" || container["name"] == i.Container {
					container["image"] = i.Image
					found = true
				}
			}
			if !found {
				return fail("container not found")
			}
		}
		manifest = v
	default:
		return fail("action must be create, image or scale")
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		return deploymentPlan{}, err
	}
	p, err := preparePlan(ctx, c, config, listen, b)
	if err != nil {
		return deploymentPlan{}, err
	}
	// preparePlan does its own read. Bind both reads, otherwise an intervening
	// edit could be assigned a fresh version while retaining our older payload.
	for _, a := range p.Actions {
		if i.Action == "create" && a.Method != "POST" {
			return fail("CONFLICT: target appeared while planning")
		}
		if i.Action != "create" && (a.UID != i.UID || a.Revision != i.Revision) {
			return fail("CONFLICT: target changed while planning")
		}
	}
	return p, nil
}

func saveAppPlan(ctx context.Context, c *agentClient, o mcpOptions, i appIntent) (string, error) {
	if i.Namespace == "" {
		i.Namespace = "default"
	}
	p, err := prepareAppPlan(ctx, c, o.Config, o.Listen, i)
	if err != nil {
		return "", err
	}
	id := planHash(p)
	if err = privateDir(o.Plans, true); err != nil {
		return "", err
	}
	if err = exclusiveJSON(filepath.Join(o.Plans, id+".json"), p); err != nil {
		return "", err
	}
	// The model needs the proposed intent and exact plan handle, not three
	// copies of every environment variable. The human review still loads the
	// complete immutable before/after via the existing local plan command.
	var targets []string
	var warnings []string
	for _, a := range p.Actions {
		targets = append(targets, planTarget(a))
		warnings = append(warnings, a.Warnings...)
	}
	b, err := json.Marshal(map[string]any{"format": "shearwater.app-plan/v1", "plan": id, "expires": p.Expires, "intent": i, "targets": targets, "warnings": warnings, "applied": false, "reviewRequired": true, "reviewCommand": "sw plan --plan=" + id, "limits": []string{"internal TCP Service only; no public URL", "multiple targets are not transactional", "TCP readiness does not verify HTTP content"}})
	return string(b), err
}

func runAppPlan(args []string, out io.Writer) error {
	f := flag.NewFlagSet("app-plan", flag.ContinueOnError)
	o := mcpOptions{}
	f.StringVar(&o.Config, "config-dir", defaultConfigDir, "server credentials")
	f.StringVar(&o.Listen, "listen", defaultListen, "local HTTPS API")
	f.StringVar(&o.Plans, "plan-dir", "", "private plans directory")
	i := appIntent{}
	f.StringVar(&i.Action, "action", "", "create, image, or scale")
	f.StringVar(&i.Name, "name", "", "application name")
	f.StringVar(&i.Namespace, "n", "default", "namespace")
	f.StringVar(&i.Image, "image", "", "prepared container image")
	f.StringVar(&i.Container, "container", "", "container to update")
	f.StringVar(&i.UID, "uid", "", "observed Deployment UID for update")
	f.StringVar(&i.Revision, "resource-version", "", "observed Deployment resourceVersion for update")
	port := f.Int("port", 0, "container and internal Service TCP port (create)")
	replicas := f.Int("replicas", 1, "replica count (create or scale)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *port < 0 || *port > 65535 || *replicas < 0 || *replicas > 1000 {
		return fmt.Errorf("port or replicas out of range")
	}
	i.Port = int32(*port)
	f.Visit(func(flag *flag.Flag) {
		if flag.Name == "replicas" {
			n := int32(*replicas)
			i.Replicas = &n
		}
	})
	if o.Plans == "" {
		o.Plans = filepath.Join(o.Config, "plans")
	}
	c, err := localAPI(o.Config, o.Listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := saveAppPlan(ctx, c, o, i)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, result)
	return err
}
