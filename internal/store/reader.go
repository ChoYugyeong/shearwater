package store

// Reader is the read side used by components that run on every node: the
// service proxy, the DNS server, network policy and the node agent. *Store
// implements it on the control plane; a worker node implements it with a
// client of the control plane's node API (docs/design-multinode-tenancy.md 3).
// Code that runs on workers must depend on this interface, not on *Store.
type Reader interface {
	Get(k Key) (Record, error)
	// List returns the objects of one resource and the revision it read at.
	// An empty namespace means all namespaces.
	List(group, resource, namespace string) ([]Record, int64, error)
	// Changed returns a channel that is closed at the next change. Take the
	// channel first and read after.
	Changed() <-chan struct{}
}

var _ Reader = (*Store)(nil)
