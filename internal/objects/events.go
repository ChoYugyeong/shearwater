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

var eventsRes = apis.Lookup("", "v1", "events")

// PodEvent writes the explanation with the state transition that caused it.
// "slot" groups repeats: one scheduling record per Pod, and separate Started
// and warning records for each container. This avoids an unbounded list for
// a crash-looping app. The owner UID makes the events follow that Pod through
// garbage collection, even if an operator creates another Pod with its name.
func PodEvent(tx *store.Tx, pod *apis.Pod, now time.Time, slot, field, reason, message, component, kind string) error {
	sum := sha256.Sum256([]byte(slot))
	name := fmt.Sprintf("%s.%x", pod.Metadata.UID, sum[:8])
	key := Key(eventsRes, pod.Metadata.Namespace, name)
	event := &apis.Event{}
	rec, err := tx.Get(key)
	create := errors.Is(err, store.ErrNotFound)
	switch {
	case create:
		event.Metadata.Name, event.Metadata.Namespace = name, pod.Metadata.Namespace
		Stamp(eventsRes, event)
		event.Metadata.OwnerReferences = []apis.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: pod.Metadata.Name, UID: pod.Metadata.UID}}
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(rec.Data, event); err != nil {
			return err
		}
	}
	at := &apis.Time{Time: now.UTC().Truncate(time.Second)}
	if create || event.Reason != reason || event.Message != message {
		event.FirstTimestamp, event.Count = at, 0
	}
	event.InvolvedObject = apis.ObjectReference{
		APIVersion: "v1", Kind: "Pod", Namespace: pod.Metadata.Namespace,
		Name: pod.Metadata.Name, UID: pod.Metadata.UID, ResourceVersion: pod.Metadata.ResourceVersion, FieldPath: field,
	}
	event.Reason, event.Message, event.EventType, event.LastTimestamp = reason, message, kind, at
	event.Source = apis.EventSource{Component: component, Host: pod.Spec.NodeName}
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
