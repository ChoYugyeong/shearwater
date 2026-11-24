// Package nodeapi is how worker nodes reach the cluster state: an HTTPS API
// under /shearwater/node/v1/ on the control plane, and a client that gives a
// worker's node agent, proxy and DNS the same interfaces the control plane's
// own node uses on SQLite (docs/design-multinode-tenancy.md 3.1).
package nodeapi

import (
	"encoding/json"

	"shearwater/internal/store"
)

// Prefix is where the node API is served.
const Prefix = "/shearwater/node/v1/"

// JoinRequest is what a server sends to join (authenticated by a join token).
type JoinRequest struct {
	NodeName   string `json:"nodeName"`
	InternalIP string `json:"internalIP"`
	CSR        string `json:"csr"` // PEM request for the node's serving certificate
}

// JoinResponse gives a joined node everything it needs to run.
type JoinResponse struct {
	NodeName    string `json:"nodeName"`
	NodeToken   string `json:"nodeToken"`
	PodCIDR     string `json:"podCIDR"`
	ClusterCIDR string `json:"clusterCIDR"`
	ServiceCIDR string `json:"serviceCIDR"`
	ServingCert string `json:"servingCert"` // PEM, signed by the cluster CA
}

type wireKey struct {
	Group     string `json:"group,omitempty"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

func toWireKey(k store.Key) wireKey    { return wireKey(k) }
func (k wireKey) key() store.Key       { return store.Key(k) }
func (k wireKey) String() string       { return k.key().String() }
func keyOf(r wireRecord) store.Key     { return r.Key.key() }
func recordKey(r store.Record) wireKey { return toWireKey(r.Key) }

type wireRecord struct {
	Key        wireKey         `json:"key"`
	UID        string          `json:"uid"`
	Rev        int64           `json:"rev"`
	Generation int64           `json:"generation,omitempty"`
	Deleting   bool            `json:"deleting,omitempty"`
	Data       json.RawMessage `json:"data"`
}

func toWire(r store.Record) wireRecord {
	return wireRecord{Key: recordKey(r), UID: r.UID, Rev: r.Rev, Generation: r.Generation, Deleting: r.Deleting, Data: r.Data}
}

func (r wireRecord) record() store.Record {
	return store.Record{Key: keyOf(r), UID: r.UID, Rev: r.Rev, Generation: r.Generation, Deleting: r.Deleting, Data: []byte(r.Data)}
}

type listResponse struct {
	Rev   int64        `json:"rev"`
	Items []wireRecord `json:"items"`
}

type wireChange struct {
	Rev  int64           `json:"rev"`
	Key  wireKey         `json:"key"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
	Prev json.RawMessage `json:"prev,omitempty"`
}

type changesResponse struct {
	Next    int64        `json:"next"`
	Changes []wireChange `json:"changes"`
}

type waitResponse struct {
	Rev int64 `json:"rev"`
}

// A transaction: the revisions the node read, and the writes it wants.
type txRead struct {
	Key    wireKey `json:"key"`
	Exists bool    `json:"exists"`
	Rev    int64   `json:"rev,omitempty"`
}

type txWrite struct {
	Op        string          `json:"op"` // create, update or delete
	Key       wireKey         `json:"key"`
	ExpectRev int64           `json:"expectRev,omitempty"`
	Data      json.RawMessage `json:"data"` // the object as written (for delete: as it was last)
}

type txRequest struct {
	Reads  []txRead  `json:"reads,omitempty"`
	Writes []txWrite `json:"writes"`
}

type txResponse struct {
	Records []wireRecord `json:"records"`
}

type allocRequest struct {
	Kind       string   `json:"kind"`
	UID        string   `json:"uid"`
	Candidates []string `json:"candidates"`
}

type allocResponse struct {
	IP string `json:"ip"`
}

type releaseRequest struct {
	UID string `json:"uid"`
}

type networkResponse struct {
	Services       []wireRecord      `json:"services"`
	EndpointSlices []wireRecord      `json:"endpointSlices"`
	Pods           []wireRecord      `json:"pods"`
	PodIPs         map[string]string `json:"podIPs"`
}

// Error codes in errorResponse.
const (
	codeConflict  = "Conflict"      // a read or an expected revision changed: run the transaction again
	codeExists    = "AlreadyExists" // store.ErrExists
	codeNotFound  = "NotFound"      // store.ErrNotFound
	codeCompacted = "Compacted"     // store.ErrCompacted
	codeForbidden = "Forbidden"
	codeInvalid   = "Invalid"
	codeAuth      = "Unauthorized"
	codeInternal  = "Internal"
)

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
