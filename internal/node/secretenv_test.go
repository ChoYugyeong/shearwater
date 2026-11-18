package node

import (
	"testing"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

var secretsRes = apis.Lookup("", "v1", "secrets")

func addSecret(e *env, namespace, name string, data map[string]string) {
	e.t.Helper()
	s := &apis.Secret{Metadata: apis.ObjectMeta{Namespace: namespace, Name: name}, StringData: data}
	apis.DefaultSecret(s)
	objects.Stamp(secretsRes, s)
	if errs := apis.ValidateSecret(s); len(errs) > 0 {
		e.t.Fatalf("test secret is invalid: %v", errs)
	}
	if err := e.st.Write(false, func(tx *store.Tx) error {
		_, err := tx.Create(objects.Put(secretsRes, s))
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func yes() *bool { t := true; return &t }

func TestSecretEnvPrecedenceAndPrefix(t *testing.T) {
	e := newEnv(t)
	addSecret(e, "default", "db", map[string]string{"PASSWORD": "s3cret", "USER": "app"})
	addSecret(e, "default", "extra", map[string]string{"USER": "override", "TOKEN": "t"})
	addSecret(e, "other", "foreign", map[string]string{"X": "must-not-leak"})
	addEnvService(e, "default", "web", "10.96.0.2", nil)
	pod := e.createPod("app", func(p *apis.Pod) {
		c := &p.Spec.Containers[0]
		c.EnvFrom = []apis.EnvFromSource{
			{SecretRef: &apis.SecretEnvSource{Name: "db"}},
			{Prefix: "X_", SecretRef: &apis.SecretEnvSource{Name: "extra"}},
			{SecretRef: &apis.SecretEnvSource{Name: "extra"}},                   // later source wins
			{SecretRef: &apis.SecretEnvSource{Name: "absent", Optional: yes()}}, // skipped
		}
		c.Env = []apis.EnvVar{
			{Name: "TOKEN", Value: "literal"}, // env wins over envFrom
			{Name: "DB_PASSWORD", ValueFrom: &apis.EnvVarSource{SecretKeyRef: &apis.SecretKeySelector{Name: "db", Key: "PASSWORD"}}},
			{Name: "MAYBE", ValueFrom: &apis.EnvVarSource{SecretKeyRef: &apis.SecretKeySelector{Name: "db", Key: "NOPE", Optional: yes()}}},
			{Name: "WEB_SERVICE_HOST", Value: "pinned"}, // explicit wins over Service links
		}
	})
	e.sync(e.agent, pod)
	checkContainerEnv(e, pod, "main", 0, []string{
		"PASSWORD=s3cret", "USER=override", "X_TOKEN=t", "X_USER=override", "TOKEN=literal",
		"DB_PASSWORD=s3cret", "WEB_SERVICE_HOST=pinned", "WEB_SERVICE_PORT=80",
	})
}

func TestMissingSecretWaitsWithConfigErrorAndNoValue(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("app", func(p *apis.Pod) {
		p.Spec.Containers[0].Env = []apis.EnvVar{
			{Name: "DB_PASSWORD", ValueFrom: &apis.EnvVarSource{SecretKeyRef: &apis.SecretKeySelector{Name: "db", Key: "PASSWORD"}}},
		}
	})
	e.sync(e.agent, pod)
	w := e.get(pod).Status.ContainerStatuses[0].State.Waiting
	if w == nil || w.Reason != "CreateContainerConfigError" || w.Message != `secret "db" not found` {
		t.Fatalf("want CreateContainerConfigError for the missing secret, got %+v", w)
	}
	addSecret(e, "default", "db", map[string]string{"OTHER": "v"})
	e.clk.advance(time.Minute) // past the start backoff
	e.sync(e.agent, pod)
	w = e.get(pod).Status.ContainerStatuses[0].State.Waiting
	if w == nil || w.Message != "couldn't find key PASSWORD in Secret default/db" {
		t.Fatalf("want the missing key named, got %+v", w)
	}
}

func TestSecretValueReachesTheNextContainerCreation(t *testing.T) {
	e := newEnv(t)
	pod := e.createPod("app", func(p *apis.Pod) {
		p.Spec.Containers[0].EnvFrom = []apis.EnvFromSource{{SecretRef: &apis.SecretEnvSource{Name: "db"}}}
	})
	addSecret(e, "default", "db", map[string]string{"PASSWORD": "late"})
	e.sync(e.agent, pod)
	checkContainerEnv(e, pod, "main", 0, []string{"PASSWORD=late"})
}
