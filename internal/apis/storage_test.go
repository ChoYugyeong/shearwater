package apis

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestStorageValidation(t *testing.T) {
	makePV := func() *PersistentVolume {
		return &PersistentVolume{
			Metadata: ObjectMeta{Name: "disk"},
			Spec: PersistentVolumeSpec{
				Capacity: map[string]Quantity{"storage": "2Gi"}, AccessModes: []string{"ReadWriteOnce"},
				Local: &LocalVolumeSource{Path: "/srv/disk"}, PersistentVolumeReclaimPolicy: "Retain",
				NodeAffinity: &VolumeNodeAffinity{Required: &NodeSelector{NodeSelectorTerms: []NodeSelectorTerm{
					{MatchFields: []NodeSelectorRequirement{{Key: "metadata.name", Operator: "In", Values: []string{"n1"}}}},
				}}},
			},
		}
	}
	if errs := validatePV(makePV()); len(errs) != 0 {
		t.Fatal(errs)
	}
	for name, change := range map[string]func(*PersistentVolume){
		"relative path": func(p *PersistentVolume) { p.Spec.Local.Path = "tmp" },
		"two sources":   func(p *PersistentVolume) { p.Spec.HostPath = &HostPathVolumeSource{Path: "/srv/disk"} },
		"no source":     func(p *PersistentVolume) { p.Spec.Local = nil },
		"affinity":      func(p *PersistentVolume) { p.Spec.NodeAffinity = nil },
		"capacity":      func(p *PersistentVolume) { p.Spec.Capacity["storage"] = "0Gi" },
		"access mode":   func(p *PersistentVolume) { p.Spec.AccessModes = []string{"bogus"} },
		"block":         func(p *PersistentVolume) { p.Spec.VolumeMode = "Block" },
		"policy":        func(p *PersistentVolume) { p.Spec.PersistentVolumeReclaimPolicy = "Recycle" },
		"phase":         func(p *PersistentVolume) { p.Status.Phase = "MadeUp" },
	} {
		t.Run(name, func(t *testing.T) {
			p := makePV()
			change(p)
			if len(validatePV(p)) == 0 {
				t.Fatal("accepted invalid PV")
			}
		})
	}
	c := &PersistentVolumeClaim{Metadata: ObjectMeta{Name: "claim"}, Spec: PersistentVolumeClaimSpec{
		AccessModes: []string{"ReadWriteOnce"}, Resources: ResourceRequirements{Requests: map[string]Quantity{"storage": "2Gi"}},
	}}
	res := Lookup("", "v1", "persistentvolumeclaims")
	res.Prepare(c)
	res.Default(c)
	if errs := res.Validate(c); len(errs) != 0 || !slices.Contains(c.Metadata.Finalizers, PVCProtection) || c.Status.Phase != "Pending" {
		t.Fatalf("prepared PVC: %+v, errors %v", c, errs)
	}
	if len(NewlyStored(c, nil)) != 1 || len(NewlyStored(c, c)) != 0 {
		t.Fatal("expected one capacity warning only when set or changed")
	}
	c.Spec.Resources.Requests["storage"] = "-1"
	if len(res.Validate(c)) == 0 {
		t.Fatal("accepted negative storage request")
	}
	sc := &StorageClass{Metadata: ObjectMeta{Name: "local-path"}, Provisioner: LocalPathProvisioner}
	class := Lookup(StorageGroup, "v1", "storageclasses")
	class.Default(sc)
	if errs := class.Validate(sc); len(errs) != 0 || sc.ReclaimPolicy != "Delete" || sc.VolumeBindingMode != "Immediate" {
		t.Fatalf("default StorageClass: %+v, errors %v", sc, errs)
	}
	sc.VolumeBindingMode = "Later"
	if len(class.Validate(sc)) == 0 {
		t.Fatal("accepted invalid binding mode")
	}
}

func TestPodClaimVolumeValidation(t *testing.T) {
	p := &Pod{Metadata: ObjectMeta{Name: "app"}, Spec: PodSpec{
		Containers: []Container{{Name: "app", Image: "app", VolumeMounts: []VolumeMount{{Name: "data", MountPath: "/data"}}}},
		Volumes:    []Volume{{Name: "data", PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "disk"}}},
	}}
	DefaultPod(p)
	if errs := ValidatePod(p); len(errs) != 0 {
		t.Fatal(errs)
	}
	p.Spec.Volumes[0].EmptyDir = &EmptyDirVolumeSource{}
	if len(ValidatePod(p)) == 0 {
		t.Fatal("accepted two sources")
	}
	p.Spec.Volumes[0].EmptyDir = nil
	p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = ""
	if len(ValidatePod(p)) == 0 {
		t.Fatal("accepted an empty claim name")
	}
}

func TestStorageStatusDecode(t *testing.T) {
	for _, obj := range []Object{&PersistentVolume{}, &PersistentVolumeClaim{}} {
		if err := json.Unmarshal([]byte(`{"status":{"phase":"Bogus"}}`), obj); err == nil {
			t.Fatalf("%T accepted an unknown phase on /status", obj)
		}
		if err := json.Unmarshal([]byte(`{"status":{"phase":"Bound"}}`), obj); err != nil {
			t.Fatal(err)
		}
	}
}
