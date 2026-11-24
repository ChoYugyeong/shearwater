package store

import "context"

// Txn is the part of a write transaction that code running on every node
// uses. *Tx implements it on the control plane. On a worker node the node API
// client implements it as an optimistic transaction: reads are remembered
// with their revisions and the control plane applies the writes only if those
// objects did not change meanwhile (docs/design-multinode-tenancy.md 3.1).
type Txn interface {
	Get(k Key) (Record, error)
	Create(p Put) (Record, error)
	Update(p Put, expectRev int64) (rec Record, changed bool, err error)
	Delete(k Key, expectRev int64, encode func(rev int64) ([]byte, error)) (Record, error)
}

var _ Txn = (*Tx)(nil)

// Backend is what a node agent needs from the cluster state.
type Backend interface {
	Reader
	Changes(group, resource, namespace string, after int64, limit int) ([]Change, int64, error)
	// Transact runs fn as one transaction. On a worker it may run fn more
	// than once when the control plane reports a conflict, so fn must read
	// what it depends on through the Txn it gets.
	Transact(fn func(Txn) error) error
	AllocIP(kind, uid string, candidates func(yield func(string) bool)) (string, error)
	ReleaseIP(uid string) error
	IPs(kind string) (map[string]string, error)
}

// NetworkReader is what the service proxy reads.
type NetworkReader interface {
	Reader
	ServiceNetwork(ctx context.Context) (NetworkView, error)
	IPs(kind string) (map[string]string, error)
	PodAtIP(ctx context.Context, namespace, name, ip string) (Record, error)
}

var (
	_ Backend       = (*Store)(nil)
	_ NetworkReader = (*Store)(nil)
)

// Transact runs fn in one write transaction (Backend).
func (s *Store) Transact(fn func(Txn) error) error {
	return s.Write(false, func(tx *Tx) error { return fn(tx) })
}
