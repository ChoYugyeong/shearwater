// Package objects holds what every writer of API objects shares: the API
// handlers, the node agent and (from M3) the controllers all turn a typed
// object into a store write the same way.
package objects

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

func Key(res *apis.Resource, namespace, name string) store.Key {
	return store.Key{Group: res.Group, Resource: res.Plural, Namespace: namespace, Name: name}
}

// Put builds the store write for obj.
func Put(res *apis.Resource, obj apis.Object) store.Put {
	m := obj.Meta()
	owners := make([]string, len(m.OwnerReferences))
	for i, o := range m.OwnerReferences {
		owners[i] = o.UID
	}
	return store.Put{
		Key: Key(res, m.Namespace, m.Name), UID: m.UID, Generation: m.Generation,
		Deleting: m.DeletionTimestamp != nil, Owners: owners, Encode: Encoder(obj),
	}
}

// Encoder returns the function the store calls to get obj's JSON at a revision.
func Encoder(obj apis.Object) func(int64) ([]byte, error) {
	return func(rev int64) ([]byte, error) {
		obj.Meta().ResourceVersion = strconv.FormatInt(rev, 10)
		return json.Marshal(obj)
	}
}

// Stamp fills in what the server owns on a new object.
func Stamp(res *apis.Resource, obj apis.Object) {
	*obj.Type() = apis.TypeMeta{Kind: res.Kind, APIVersion: res.APIVersion()}
	m := obj.Meta()
	m.UID = NewUID()
	m.CreationTimestamp = apis.Now()
	m.Generation = 1
	m.ResourceVersion = ""
	m.DeletionTimestamp, m.DeletionGracePeriodSeconds = nil, nil
	if res.Prepare != nil {
		res.Prepare(obj)
	}
	if res.Default != nil {
		res.Default(obj)
	}
}

// NewUID returns a random (version 4) UUID.
func NewUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
