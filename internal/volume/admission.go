// Package volume binds filesystem claims and provisions node-local volumes.
package volume

import (
	"encoding/json"
	"errors"

	"shearwater/internal/admission"
	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

var (
	pvRes    = apis.Lookup("", "v1", "persistentvolumes")
	pvcRes   = apis.Lookup("", "v1", "persistentvolumeclaims")
	classRes = apis.Lookup(apis.StorageGroup, "v1", "storageclasses")
)

func Bootstrap(st *store.Store) error {
	return st.Write(false, func(tx *store.Tx) error {
		key := objects.Key(classRes, "", "local-path")
		if _, err := tx.Get(key); !errors.Is(err, store.ErrNotFound) {
			return err
		}
		sc := &apis.StorageClass{
			Metadata:    apis.ObjectMeta{Name: key.Name, Annotations: map[string]string{apis.DefaultClassAnnotation: "true"}},
			Provisioner: apis.LocalPathProvisioner, ReclaimPolicy: "Delete", VolumeBindingMode: "WaitForFirstConsumer",
		}
		objects.Stamp(classRes, sc)
		_, err := tx.Create(objects.Put(classRes, sc))
		return err
	})
}

func Admit(r admission.Request) error {
	c, ok := r.Object.(*apis.PersistentVolumeClaim)
	if !ok || r.Operation != admission.Create || c.Spec.StorageClassName != nil {
		return nil
	}
	records, err := r.Tx.ListObjects("")
	if err != nil {
		return err
	}
	// Kubernetes selects the newest default class when administrators overlap
	// two defaults during migration. Break timestamp ties by name.
	var chosen *apis.StorageClass
	for _, rec := range records {
		if rec.Key.Group != classRes.Group || rec.Key.Resource != classRes.Plural || rec.Deleting {
			continue
		}
		var sc apis.StorageClass
		if err := json.Unmarshal(rec.Data, &sc); err != nil {
			return err
		}
		if sc.Metadata.Annotations[apis.DefaultClassAnnotation] == "true" &&
			(chosen == nil || sc.Metadata.CreationTimestamp.After(chosen.Metadata.CreationTimestamp.Time)) {
			chosen = &sc
		}
	}
	if chosen != nil {
		c.Spec.StorageClassName = &chosen.Metadata.Name
	}
	return nil
}
