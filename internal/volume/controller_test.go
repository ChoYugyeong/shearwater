package volume

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"shearwater/internal/admission"
	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

type fixture struct {
	*testing.T
	Controller
}

func setup(t *testing.T) fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := Bootstrap(st); err != nil {
		t.Fatal(err)
	}
	return fixture{t, Controller{Store: st}}
}

func (f fixture) create(res *apis.Resource, obj apis.Object) {
	f.Helper()
	objects.Stamp(res, obj)
	err := f.Store.Write(false, func(tx *store.Tx) error {
		if err := Admit(admission.Request{Operation: admission.Create, Tx: tx, Object: obj}); err != nil {
			return err
		}
		_, err := tx.Create(objects.Put(res, obj))
		return err
	})
	if err != nil {
		f.Fatal(err)
	}
}

func (f fixture) fetch(res *apis.Resource, obj apis.Object) {
	f.Helper()
	key := objects.Key(res, obj.Meta().Namespace, obj.Meta().Name)
	rec, err := f.Store.Get(key)
	if err != nil {
		f.Fatal(err)
	}
	// Unmarshal into a fresh value so omitted fields cannot retain old data.
	fresh := res.New()
	if err := json.Unmarshal(rec.Data, fresh); err != nil {
		f.Fatal(err)
	}
	switch target := obj.(type) {
	case *apis.PersistentVolume:
		*target = *fresh.(*apis.PersistentVolume)
	case *apis.PersistentVolumeClaim:
		*target = *fresh.(*apis.PersistentVolumeClaim)
	case *apis.StorageClass:
		*target = *fresh.(*apis.StorageClass)
	default:
		f.Fatalf("unexpected test object %T", obj)
	}
}

func (f fixture) remove(res *apis.Resource, obj apis.Object) {
	f.Helper()
	err := f.Store.Write(false, func(tx *store.Tx) error {
		rec, err := tx.Get(objects.Key(res, obj.Meta().Namespace, obj.Meta().Name))
		if err != nil {
			return err
		}
		current := res.New()
		if err := json.Unmarshal(rec.Data, current); err != nil {
			return err
		}
		_, _, err = objects.Delete(tx, res, rec, current, nil, time.Now())
		return err
	})
	if err != nil {
		f.Fatal(err)
	}
}

func (f fixture) sync() {
	f.Helper()
	if err := f.Sync(); err != nil {
		f.Fatal(err)
	}
}

func claim(class *string) *apis.PersistentVolumeClaim {
	return &apis.PersistentVolumeClaim{
		Metadata: apis.ObjectMeta{Namespace: "team", Name: "data"},
		Spec: apis.PersistentVolumeClaimSpec{StorageClassName: class, AccessModes: []string{"ReadWriteOnce"},
			Resources: apis.ResourceRequirements{Requests: map[string]apis.Quantity{"storage": "1Gi"}}},
	}
}

func staticPV(name, policy string) *apis.PersistentVolume {
	return &apis.PersistentVolume{
		Metadata: apis.ObjectMeta{Name: name},
		Spec: apis.PersistentVolumeSpec{
			Capacity: map[string]apis.Quantity{"storage": "1024Mi"}, AccessModes: []string{"ReadWriteOnce"},
			PersistentVolumeReclaimPolicy: policy, HostPath: &apis.HostPathVolumeSource{Path: "/srv/" + name},
			NodeAffinity: &apis.VolumeNodeAffinity{Required: &apis.NodeSelector{NodeSelectorTerms: []apis.NodeSelectorTerm{
				{MatchFields: []apis.NodeSelectorRequirement{{Key: "metadata.name", Operator: "In", Values: []string{"n1"}}}},
			}}},
		},
	}
}

func consumer(node string) *apis.Pod {
	return &apis.Pod{Metadata: apis.ObjectMeta{Name: "app", Namespace: "team"}, Spec: apis.PodSpec{
		NodeName: node, Volumes: []apis.Volume{{Name: "disk", PersistentVolumeClaim: &apis.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}},
	}}
}

func TestStaticBindingAndReclaim(t *testing.T) {
	for _, policy := range []string{"Delete", "Retain"} {
		t.Run(policy, func(t *testing.T) {
			f := setup(t)
			empty := ""
			c := claim(&empty)
			f.create(pvcRes, c)
			for _, name := range []string{"wrong-class", "reserved", "too-small", "wrong-mode"} {
				pv := staticPV(name, policy)
				switch name {
				case "wrong-class":
					pv.Spec.StorageClassName = "slow"
				case "reserved":
					pv.Spec.ClaimRef = &apis.ObjectReference{Namespace: "other", Name: "data"}
				case "too-small":
					pv.Spec.Capacity["storage"] = "1G"
				case "wrong-mode":
					pv.Spec.AccessModes = []string{"ReadOnlyMany"}
				}
				f.create(pvRes, pv)
			}
			f.sync()
			f.fetch(pvcRes, c)
			if c.Status.Phase != "Pending" || c.Spec.VolumeName != "" {
				t.Fatalf("incompatible volumes bound: %+v", c)
			}
			pv := staticPV("fit", policy)
			f.create(pvRes, pv)
			f.sync()
			f.fetch(pvcRes, c)
			f.fetch(pvRes, pv)
			if c.Spec.VolumeName != "fit" || c.Status.Phase != "Bound" || c.Status.Capacity["storage"] != "1024Mi" ||
				!slices.Equal(c.Status.AccessModes, pv.Spec.AccessModes) || !pv.Claims(c) || pv.Status.Phase != "Bound" {
				t.Fatalf("incomplete bind: %+v %+v", c, pv)
			}
			rev := c.Metadata.ResourceVersion
			f.sync()
			f.fetch(pvcRes, c)
			if c.Metadata.ResourceVersion != rev {
				t.Fatal("idle sync rewrote claim")
			}
			f.remove(pvcRes, c)
			f.sync()
			if _, err := f.Store.Get(objects.Key(pvcRes, "team", "data")); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unused PVC not deleted: %v", err)
			}
			if policy == "Retain" {
				f.fetch(pvRes, pv)
				if pv.Status.Phase != "Released" || !pv.Claims(c) {
					t.Fatalf("retained PV: %+v", pv)
				}
				return
			}
			if _, err := f.Store.Get(objects.Key(pvRes, "", "fit")); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("Delete policy: %v", err)
			}
		})
	}
}

func TestProvisionScheduleAndProtect(t *testing.T) {
	f := setup(t)
	c := claim(nil)
	p := consumer("")
	pods := apis.Lookup("", "v1", "pods")
	nodes := apis.Lookup("", "v1", "nodes")
	n1 := &apis.Node{Metadata: apis.ObjectMeta{Name: "n1", Labels: map[string]string{"kubernetes.io/hostname": "n1"}}}
	n2 := &apis.Node{Metadata: apis.ObjectMeta{Name: "n2", Labels: map[string]string{"kubernetes.io/hostname": "n2"}}}
	f.create(nodes, n1)
	f.create(nodes, n2)
	f.create(pods, p)
	filter := SchedulerFilter(f.Store)
	if got := filter(p, n1); got != "persistentvolumeclaim \"data\" not found" {
		t.Fatalf("missing: %q", got)
	}
	f.create(pvcRes, c)
	f.sync()
	if c.ClassName() != "local-path" || filter(p, n1) != "" || filter(p, n2) != "" {
		t.Fatal("unbound default claim should fit either node")
	}
	second := consumer("")
	second.Metadata.Name, second.Metadata.UID = "another-consumer", objects.NewUID()
	if why := filter(second, n1); why != noVolume {
		t.Fatalf("two Pods chose placement for one unbound claim in one pass: %q", why)
	}
	name := "pvc-" + c.Metadata.UID
	if _, err := f.Store.Get(objects.Key(pvRes, "", name)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("provisioned before scheduling: %v", err)
	}
	p.Spec.NodeName = "n2"
	writePod := func() {
		t.Helper()
		if err := f.Store.Write(false, func(tx *store.Tx) error {
			_, _, err := tx.Update(objects.Put(pods, p), 0)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	writePod()
	if filter(second, n1) != conflict || filter(second, n2) != "" {
		t.Fatal("next pass must follow the first consumer even before binding")
	}
	f.sync()
	pv := &apis.PersistentVolume{Metadata: apis.ObjectMeta{Name: name}}
	f.fetch(pvRes, pv)
	f.fetch(pvcRes, c)
	if pv.Spec.Local == nil || pv.Spec.Local.Path != apis.LocalVolumeRoot+"/"+name ||
		pv.Spec.Capacity["storage"] != "1Gi" || !pv.Claims(c) || c.Spec.VolumeName != name || c.Status.Phase != "Bound" {
		t.Fatalf("provisioned PV/PVC: %+v %+v", pv, c)
	}
	if filter(p, n2) != "" || filter(p, n1) != "node(s) had volume node affinity conflict" {
		t.Fatal("bound claim must follow its node")
	}
	f.remove(pvcRes, c)
	f.sync()
	f.fetch(pvcRes, c)
	if c.Metadata.DeletionTimestamp == nil || !slices.Contains(c.Metadata.Finalizers, apis.PVCProtection) {
		t.Fatal("in-use claim lost protection")
	}
	p.Status.Phase = "Succeeded"
	writePod()
	f.sync()
	if _, err := f.Store.Get(objects.Key(pvcRes, "team", "data")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("terminal consumer held claim: %v", err)
	}
}

func TestImmediatePendingAndDefaultClass(t *testing.T) {
	f := setup(t)
	sc := &apis.StorageClass{Metadata: apis.ObjectMeta{Name: "local-path"}}
	f.fetch(classRes, sc)
	uid := sc.Metadata.UID
	if err := Bootstrap(f.Store); err != nil {
		t.Fatal(err)
	}
	f.fetch(classRes, sc)
	if sc.Metadata.UID != uid || sc.Metadata.Annotations[apis.DefaultClassAnnotation] != "true" || sc.Provisioner != apis.LocalPathProvisioner {
		t.Fatalf("default class: %+v", sc)
	}
	empty := ""
	c := claim(&empty)
	f.create(pvcRes, c)
	if c.ClassName() != "" {
		t.Fatal("explicit classless claim was defaulted")
	}
	filter := SchedulerFilter(f.Store)
	if why := filter(consumer(""), &apis.Node{}); why != "node(s) didn't find available persistent volumes to bind" {
		t.Fatalf("immediate claim without PV: %q", why)
	}
}
