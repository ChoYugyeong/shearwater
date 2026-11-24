package objects

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

// repeatQuiet is how long the same event about an object is not written
// again. A controller that keeps failing the same way (for example a quota
// refusing a ReplicaSet's next Pod) would otherwise write on every pass, and
// every write wakes the controllers for another pass.
const repeatQuiet = time.Minute

// ObjectEvent records an event about any object, grouped by slot like
// PodEvent. An identical reason and message written less than a minute ago
// is not written again.
func ObjectEvent(tx store.Txn, res *apis.Resource, m *apis.ObjectMeta, now time.Time, slot, reason, message, component, kind string) error {
	sum := sha256.Sum256([]byte(slot))
	name := fmt.Sprintf("%s.%x", m.UID, sum[:8])
	key := Key(eventsRes, m.Namespace, name)
	event := &apis.Event{}
	rec, err := tx.Get(key)
	create := errors.Is(err, store.ErrNotFound)
	switch {
	case create:
		event.Metadata.Name, event.Metadata.Namespace = name, m.Namespace
		Stamp(eventsRes, event)
		event.Metadata.OwnerReferences = []apis.OwnerReference{{APIVersion: res.APIVersion(), Kind: res.Kind, Name: m.Name, UID: m.UID}}
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(rec.Data, event); err != nil {
			return err
		}
		if event.Reason == reason && event.Message == message && event.LastTimestamp != nil && now.Sub(event.LastTimestamp.Time) < repeatQuiet {
			return nil
		}
	}
	at := &apis.Time{Time: now.UTC().Truncate(time.Second)}
	if create || event.Reason != reason || event.Message != message {
		event.FirstTimestamp, event.Count = at, 0
	}
	event.InvolvedObject = apis.ObjectReference{APIVersion: res.APIVersion(), Kind: res.Kind, Namespace: m.Namespace,
		Name: m.Name, UID: m.UID, ResourceVersion: m.ResourceVersion}
	event.Reason, event.Message, event.EventType, event.LastTimestamp = reason, message, kind, at
	event.Source = apis.EventSource{Component: component}
	if event.Count < math.MaxInt32 {
		event.Count++
	}
	if create {
		_, err = tx.Create(Put(eventsRes, event))
	} else {
		_, _, err = tx.Update(Put(eventsRes, event), rec.Rev)
	}
	return err
}
