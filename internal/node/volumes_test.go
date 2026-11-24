package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
	"shearwater/internal/volume"
)

func TestClaimMountAndCollection(t *testing.T) {
	e := newEnv(t)
	a := e.agent
	a.init()
	write := func(res *apis.Resource, obj apis.Object, create bool) {
		t.Helper()
		err := e.st.Write(false, func(tx *store.Tx) error {
			if create {
				objects.Stamp(res, obj)
				_, err := tx.Create(objects.Put(res, obj))
				return err
			}
			_, _, err := tx.Update(objects.Put(res, obj), 0)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	node := &apis.Node{Metadata: apis.ObjectMeta{Name: a.NodeName, Labels: map[string]string{"kubernetes.io/hostname": a.NodeName}}}
	write(nodesRes, node, true)
	res := apis.Lookup("", "v1", "persistentvolumeclaims")
	volumes := apis.Lookup("", "v1", "persistentvolumes")
	if err := volume.Bootstrap(e.st); err != nil {
		t.Fatal(err)
	}
	class := "local-path"
	claim := &apis.PersistentVolumeClaim{Metadata: apis.ObjectMeta{Name: "claim", Namespace: "default"}, Spec: apis.PersistentVolumeClaimSpec{
		StorageClassName: &class, AccessModes: []string{"ReadWriteOnce"}, Resources: apis.ResourceRequirements{Requests: map[string]apis.Quantity{"storage": "1Gi"}},
	}}
	write(res, claim, true)
	pod := e.createPod("worker", func(p *apis.Pod) {
		p.Spec.Volumes = []apis.Volume{{Name: "data", PersistentVolumeClaim: &apis.PersistentVolumeClaimVolumeSource{ClaimName: "claim", ReadOnly: true}}}
		p.Spec.Containers[0].VolumeMounts = []apis.VolumeMount{{Name: "data", MountPath: "/data"}}
	})
	if retry, err := a.SyncPod(context.Background(), pod.Metadata.UID); err == nil || retry == 0 {
		t.Fatalf("unbound claim should retry: %v %v", retry, err)
	}
	status := e.get(pod).Status.ContainerStatuses
	if len(status) != 1 || status[0].State.Waiting == nil || !strings.Contains(status[0].State.Waiting.Message, "not bound") {
		t.Fatalf("expected waiting status for unbound claim: %+v", status)
	}
	if err := (volume.Controller{Store: e.st}).Sync(); err != nil {
		t.Fatal(err)
	}
	mounts, err := a.ensurePodFiles(pod, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(a.DataDir, "volumes")
	dir := filepath.Join(root, "pvc-"+claim.Metadata.UID)
	if mounts["data"] != dir {
		t.Fatalf("mount source %q, want %q", mounts["data"], dir)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() || st.Mode().Perm() != 0o755 {
		t.Fatalf("volume mode: %v %v", st, err)
	}
	cfg, err := a.containerConfig(pod, &pod.Spec.Containers[0], 0, mounts)
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Mounts[len(cfg.Mounts)-1]
	if m.Source != dir || m.Destination != "/data" || !m.ReadOnly {
		t.Fatalf("claim-level readOnly lost: %+v", m)
	}
	file := filepath.Join(dir, "data")
	if err := os.WriteFile(file, []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A node with different labels must reject even a preassigned Pod.
	other := &apis.Node{Metadata: apis.ObjectMeta{Name: "other", Labels: map[string]string{"kubernetes.io/hostname": "other"}}}
	write(nodesRes, other, true)
	a.NodeName = "other"
	if _, err := a.ensurePodFiles(pod, "10.0.0.1"); err == nil || !strings.Contains(err.Error(), "affinity") {
		t.Fatalf("wrong-node mount: %v", err)
	}
	a.NodeName = node.Metadata.Name
	var pv apis.PersistentVolume
	if err := a.readVolumeObject(store.Key{Resource: volumes.Plural, Name: filepath.Base(dir)}, &pv); err != nil {
		t.Fatal(err)
	}
	pv.Spec.ClaimRef.UID = objects.NewUID()
	write(volumes, &pv, false)
	if _, err := a.ensurePodFiles(pod, "10.0.0.1"); err == nil {
		t.Fatal("mounted PV reserved for another claim UID")
	}
	pv.Status.Phase = "Released"
	write(volumes, &pv, false)
	// Static PV names need not be the basename of their host directory.
	// Here its parent also looks like an orphan of the local provisioner.
	staticParent := filepath.Join(root, "pvc-static")
	if err := os.Mkdir(staticParent, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "archive")
	if err := os.Symlink(staticParent, alias); err != nil {
		t.Fatal(err)
	}
	staticDir := filepath.Join(alias, "dataset")
	static := &apis.PersistentVolume{Metadata: apis.ObjectMeta{Name: "archive"},
		Spec: apis.PersistentVolumeSpec{HostPath: &apis.HostPathVolumeSource{Path: staticDir},
			NodeAffinity: pv.Spec.NodeAffinity, AccessModes: []string{"ReadWriteOnce"}, Capacity: pv.Spec.Capacity}}
	write(volumes, static, true)
	empty := ""
	second := &apis.PersistentVolumeClaim{Metadata: apis.ObjectMeta{Name: "archive", Namespace: "default"},
		Spec: apis.PersistentVolumeClaimSpec{StorageClassName: &empty, VolumeName: "archive",
			Resources: claim.Spec.Resources, AccessModes: []string{"ReadWriteOnce"}}}
	write(res, second, true)
	if err := (volume.Controller{Store: e.st}).Sync(); err != nil {
		t.Fatal(err)
	}
	if got, err := a.claimDirectory("default", "archive"); err != nil || got != staticDir {
		t.Fatalf("static hostPath mount: %q %v", got, err)
	}
	orphan := filepath.Join(root, "pvc-"+objects.NewUID())
	keep := filepath.Join(root, "manual")
	for _, name := range []string{orphan, keep} {
		if err := os.Mkdir(name, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.collectVolumes(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "saved" {
		t.Fatalf("lost retained volume: %q %v", data, err)
	}
	// A directory without a volume object stays: after restoring an older
	// backup the objects can be missing while the data is still wanted.
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("an orphan directory was removed: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staticDir); err != nil {
		t.Fatalf("live static hostPath collected: %v", err)
	}

	// The control plane marks a released Delete volume; then its node
	// removes the data and the object.
	if err := a.readVolumeObject(store.Key{Resource: volumes.Plural, Name: filepath.Base(dir)}, &pv); err != nil {
		t.Fatal(err)
	}
	pv.Metadata.Annotations = map[string]string{apis.DeleteDataAnnotation: "true"}
	write(volumes, &pv, false)
	if err := a.collectVolumes(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released Delete volume still on disk: %v", err)
	}
	if _, err := e.st.Get(store.Key{Resource: volumes.Plural, Name: filepath.Base(dir)}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("released volume object not removed after its data: %v", err)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("an orphan directory was removed with the released one: %v", err)
	}
}

type failedVolumeList struct{ store.Backend }

func (f failedVolumeList) List(group, resource, namespace string) ([]store.Record, int64, error) {
	return nil, 0, errors.New("node API unavailable")
}

func TestVolumeGCAbortsOnReadFailure(t *testing.T) {
	e := newEnv(t)
	e.agent.init()
	e.agent.syncNode(context.Background())
	root := filepath.Join(e.agent.DataDir, "volumes")
	dir := filepath.Join(root, "pvc-"+objects.NewUID())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.agent.Store = failedVolumeList{e.st}
	if err := e.agent.collectVolumes(); err == nil {
		t.Fatal("expected read failure")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("GC removed data after a failed read", err)
	}
}
