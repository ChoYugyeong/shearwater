package node

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/runtime"
	"shearwater/internal/store"
)

var envServiceResource = apis.Lookup("", "v1", "services")

func addEnvService(e *env, namespace, name, ip string, change func(*apis.Service)) *apis.Service {
	e.t.Helper()
	svc := &apis.Service{
		Metadata: apis.ObjectMeta{Namespace: namespace, Name: name},
		Spec:     apis.ServiceSpec{ClusterIP: ip, Ports: []apis.ServicePort{{Port: 80}}},
	}
	objects.Stamp(envServiceResource, svc)
	if change != nil {
		change(svc)
		apis.DefaultService(svc)
	}
	if err := e.st.Write(false, func(tx *store.Tx) error {
		_, err := tx.Create(objects.Put(envServiceResource, svc))
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return svc
}

func checkContainerEnv(e *env, pod *apis.Pod, name string, attempt int, want []string) {
	e.t.Helper()
	cfg, ok := e.fake.Config(runtime.ContainerID(pod.Metadata.UID, name, attempt))
	if !ok {
		e.t.Fatalf("%s attempt %d did not start", name, attempt)
	}
	if !slices.Equal(cfg.Env, want) {
		e.t.Errorf("%s attempt %d env = %q, want %q", name, attempt, cfg.Env, want)
	}
}

func TestServiceEnvScopeAndExplicitOverrides(t *testing.T) {
	e := newEnv(t)
	addEnvService(e, "default", "api-web", "10.96.0.2", func(s *apis.Service) {
		s.Spec.Ports = []apis.ServicePort{
			{Name: "admin", Port: 9443, TargetPort: apis.Int(8443)},
			{Name: "web-http", Port: 80, TargetPort: apis.Str("http")},
		}
	})
	addEnvService(e, "default", "db", "10.96.0.3", nil) // a selectorless Service still has a frontend
	addEnvService(e, "other", "foreign", "10.96.0.4", nil)
	addEnvService(e, "default", "headless", "None", nil)
	addEnvService(e, "default", "unassigned", "", nil)
	addEnvService(e, "default", "bad-ip", "10.244.0.2", nil)
	addEnvService(e, "default", "ipv6", "fd00::1", nil)
	addEnvService(e, "default", "deleting", "10.96.0.5", func(s *apis.Service) { s.Metadata.DeletionTimestamp = apis.Now() })
	addEnvService(e, "default", "unsupported", "10.96.0.6", func(s *apis.Service) { s.Spec.Type = "NodePort" })
	addEnvService(e, "default", "udp", "10.96.0.7", func(s *apis.Service) { s.Spec.Ports[0].Protocol = "UDP" })

	pod := e.createPod("client", func(p *apis.Pod) {
		p.Spec.Containers[0].Env = []apis.EnvVar{
			{Name: "API_WEB_SERVICE_HOST", Value: "custom-host"},
			{Name: "API_WEB_SERVICE_PORT_WEB_HTTP", Value: ""},
			{Name: "EXTRA", Value: "literal"},
		}
		p.Spec.Containers = append(p.Spec.Containers, apis.Container{Name: "sidecar", Image: "busybox:1.36"})
	})
	e.sync(e.agent, pod)
	checkContainerEnv(e, pod, "main", 0, []string{
		"API_WEB_SERVICE_HOST=custom-host", "API_WEB_SERVICE_PORT_WEB_HTTP=", "EXTRA=literal",
		"API_WEB_SERVICE_PORT=9443", "API_WEB_SERVICE_PORT_ADMIN=9443", "DB_SERVICE_HOST=10.96.0.3", "DB_SERVICE_PORT=80",
	})
	checkContainerEnv(e, pod, "sidecar", 0, []string{
		"API_WEB_SERVICE_HOST=10.96.0.2", "API_WEB_SERVICE_PORT=9443", "API_WEB_SERVICE_PORT_ADMIN=9443",
		"API_WEB_SERVICE_PORT_WEB_HTTP=80", "DB_SERVICE_HOST=10.96.0.3", "DB_SERVICE_PORT=80",
	})
}

type startHookRuntime struct {
	runtime.Runtime
	started func(runtime.Config)
}

func (r startHookRuntime) Start(ctx context.Context, cfg runtime.Config) error {
	if err := r.Runtime.Start(ctx, cfg); err != nil {
		return err
	}
	r.started(cfg)
	return nil
}

func TestServiceEnvSnapshotSharedAcrossContainerAndAgentRestarts(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("client", func(p *apis.Pod) {
		p.Spec.Containers = append(p.Spec.Containers, apis.Container{Name: "sidecar", Image: "busybox:1.36"})
	})
	// The Pod exists while its image is still unavailable. A Service created
	// before the first start can still appear in the snapshot.
	e.fake.PullErr = errors.New("image unavailable")
	delay := e.sync(e.agent, pod)
	if delay <= 0 {
		t.Fatal("want image pull retry")
	}
	api := addEnvService(e, "default", "api", "10.96.0.2", nil)
	e.fake.PullErr = nil
	e.clk.advance(delay)
	e.agent.Runtime = startHookRuntime{Runtime: e.fake, started: func(cfg runtime.Config) {
		if cfg.Name != "main" {
			return
		}
		// This update lands between the first and second container starts.
		api.Spec.Ports[0].Port = 81
		if err := e.st.Write(false, func(tx *store.Tx) error {
			_, _, err := tx.Update(objects.Put(envServiceResource, api), 0)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		addEnvService(e, "default", "late", "10.96.0.3", nil)
	}}
	e.sync(e.agent, pod)
	want := []string{"API_SERVICE_HOST=10.96.0.2", "API_SERVICE_PORT=80"}
	checkContainerEnv(e, pod, "main", 0, want)
	checkContainerEnv(e, pod, "sidecar", 0, want)

	// A new Agent has no memory of the Pod. It opens the same data directory
	// and starts the next attempt with the saved environment.
	fresh := e.newAgent()
	fresh.DataDir = e.agent.DataDir
	e.fake.Exit(id(pod, 0), 1)
	e.sync(fresh, pod)
	checkContainerEnv(e, pod, "main", 1, want)
	newPod := e.createPod("new-client", nil)
	e.sync(fresh, newPod)
	checkContainerEnv(e, newPod, "main", 0, []string{
		"API_SERVICE_HOST=10.96.0.2", "API_SERVICE_PORT=81", "LATE_SERVICE_HOST=10.96.0.3", "LATE_SERVICE_PORT=80",
	})
}

func TestServiceEnvEmptySnapshotAndLegacyPod(t *testing.T) {
	for _, name := range []string{"empty snapshot", "legacy Pod"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			pod := e.createPod("client", nil)
			e.sync(e.agent, pod)
			if name == "legacy Pod" {
				// Simulate a Pod started before this version wrote snapshots.
				path := filepath.Join(e.agent.podDir(pod.Metadata.UID), "service-env.json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			addEnvService(e, "default", "late", "10.96.0.2", nil)
			e.fake.Exit(id(pod, 0), 1)
			fresh := e.newAgent()
			fresh.DataDir = e.agent.DataDir
			e.sync(fresh, pod)
			checkContainerEnv(e, pod, "main", 1, nil)
		})
	}
}

func TestServiceEnvSnapshotCannotBeOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service-env.json")
	want := map[string]string{"WEB_SERVICE_HOST": "10.96.0.2"}
	if err := writeServiceEnv(path, want); err != nil {
		t.Fatal(err)
	}
	if err := writeServiceEnv(path, map[string]string{"WEB_SERVICE_HOST": "10.96.0.3"}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second publisher must see an existing snapshot, got %v", err)
	}
	got, err := readServiceEnv(path)
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("snapshot = %v, error %v, want %v", got, err, want)
	}
}
