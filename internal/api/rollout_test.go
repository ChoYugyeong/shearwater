package api

import (
	"encoding/json"
	"testing"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

func TestControllerOwnsRolloutAnnotations(t *testing.T) {
	c := newClient(t)
	path := createScaleWorkload(c, deploys, "Deployment")
	key := apis.DeploymentRevisionAnnotation
	rev := c.rev()
	r := c.want(c.do("PATCH", path, `{"metadata":{"annotations":{"deployment.kubernetes.io/revision":"50"}}}`,
		"Content-Type", mergePatchType), 200)
	if r.at("metadata", "annotations", key) != nil || c.rev() != rev {
		t.Fatalf("client revision unexpectedly changed the Deployment: %s", r.raw)
	}

	res := apis.Lookup("apps", "v1", "deployments")
	if err := c.srv.Store.Write(false, func(tx *store.Tx) error {
		rec, err := tx.Get(keyOf(res, "default", "web"))
		if err != nil {
			return err
		}
		d := &apis.Deployment{}
		if err := json.Unmarshal(rec.Data, d); err != nil {
			return err
		}
		d.Metadata.Annotations[key] = "1"
		_, _, err = tx.Update(put(res, d), rec.Rev)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r = c.want(c.do("PATCH", path,
		`{"metadata":{"annotations":{"deployment.kubernetes.io/revision":null,"kubernetes.io/change-cause":"client note"}}}`,
		"Content-Type", mergePatchType), 200)
	if r.at("metadata", "annotations", key) != "1" || r.at("metadata", "annotations", "kubernetes.io/change-cause") != "client note" {
		t.Fatalf("patch should retain the revision and change the client annotation: %s", r.raw)
	}

	r = c.want(c.do("POST", pods, `{"metadata":{"name":"loose","annotations":{"shearwater.io/deployment-uid":"forged"}},
		"spec":{"containers":[{"name":"app","image":"busybox:1.36"}]}}`), 201)
	if r.at("metadata", "annotations", apis.PodDeploymentUIDAnnotation) != nil {
		t.Fatalf("standalone Pod carried a client-assigned Deployment UID: %s", r.raw)
	}
}
