package objects

import (
	"encoding/json"
	"slices"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

const ForegroundFinalizer = "foregroundDeletion"

// Orphan detaches this owner's UID from every dependent in the same transaction.
func Orphan(tx *store.Tx, ownerUID string) error {
	records, err := tx.Objects()
	if err != nil {
		return err
	}
	for _, rec := range records {
		if rec.UID == ownerUID {
			continue
		}
		res := apis.Lookup(rec.Key.Group, version(rec.Key.Group), rec.Key.Resource)
		if res == nil {
			continue
		}
		obj := res.New()
		if err := json.Unmarshal(rec.Data, obj); err != nil {
			return err
		}
		refs := obj.Meta().OwnerReferences
		refs = slices.DeleteFunc(refs, func(ref apis.OwnerReference) bool { return ref.UID == ownerUID })
		if len(refs) != len(obj.Meta().OwnerReferences) {
			obj.Meta().OwnerReferences = refs
			if _, _, err := tx.Update(Put(res, obj), rec.Rev); err != nil {
				return err
			}
		}
	}
	return nil
}

func version(group string) string {
	for _, res := range apis.Resources {
		if res.Group == group {
			return res.Version
		}
	}
	return ""
}

// Delete applies the same Pod grace and finalizer rules to API and controller deletes.
func Delete(tx *store.Tx, res *apis.Resource, rec store.Record, obj apis.Object, nodeName string,
	requestedGrace *int64, now time.Time) (store.Record, bool, error) {
	m := obj.Meta()
	var grace int64
	if pod, ok := obj.(*apis.Pod); ok && nodeName != "" && pod.Spec.NodeName == nodeName {
		grace = 30
		if g := pod.Spec.TerminationGracePeriodSeconds; g != nil {
			grace = *g
		}
		if requestedGrace != nil {
			grace = *requestedGrace
		}
		grace = min(grace, apis.MaxGraceSeconds)
	}
	if grace > 0 || len(m.Finalizers) > 0 {
		deadline := now.UTC().Add(time.Duration(grace) * time.Second).Truncate(time.Second)
		if m.DeletionTimestamp == nil || deadline.Before(m.DeletionTimestamp.Time) ||
			(grace == 0 && requestedGrace != nil) {
			m.DeletionTimestamp = &apis.Time{Time: deadline}
			m.DeletionGracePeriodSeconds = &grace
		}
		updated, _, err := tx.Update(Put(res, obj), rec.Rev)
		return updated, true, err
	}
	deleted, err := tx.Delete(rec.Key, rec.Rev, Encoder(obj))
	return deleted, false, err
}
