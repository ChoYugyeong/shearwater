package nodeapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"shearwater/internal/pki"
	"shearwater/internal/store"
)

// Client is a worker node's view of the cluster state through the control
// plane. It implements store.Backend (node agent) and store.NetworkReader
// (service proxy), so those run unchanged on a worker.
type Client struct {
	URL     string // https://host:port of the control plane
	Token   string // the node credential
	PodCIDR netip.Prefix
	HTTP    *http.Client
	Logf    func(format string, args ...any)

	mu   sync.Mutex
	wake chan struct{}
	rev  int64
	// lastOK is when the control plane last answered (status reporting).
	lastOK time.Time
}

var (
	_ store.Backend       = (*Client)(nil)
	_ store.NetworkReader = (*Client)(nil)
)

// NewClient returns a client that trusts only the cluster CA's API server.
func NewClient(baseURL, token string, caPEM []byte, podCIDR netip.Prefix) (*Client, error) {
	cfg, err := pki.PinnedTLS(caPEM, pki.ServerName)
	if err != nil {
		return nil, err
	}
	return &Client{URL: baseURL, Token: token, PodCIDR: podCIDR, wake: make(chan struct{}),
		HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, MaxIdleConnsPerHost: 8,
			IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 70 * time.Second}}}, nil
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "node api client: "+format+"\n", args...)
}

// Error is an answer of the control plane that was not a success.
type Error struct {
	Status int
	Code   string
	Msg    string
}

func (e *Error) Error() string { return fmt.Sprintf("control plane: %s (%s)", e.Msg, e.Code) }

func (e *Error) Unwrap() error {
	switch e.Code {
	case codeNotFound:
		return store.ErrNotFound
	case codeExists:
		return store.ErrExists
	case codeConflict:
		return store.ErrConflict
	case codeCompacted:
		return store.ErrCompacted
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, op string, q url.Values, in, out any) error {
	u := c.URL + Prefix + op
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.lastOK = time.Now()
	c.mu.Unlock()
	if resp.StatusCode >= 300 {
		var e errorResponse
		if json.Unmarshal(data, &e) != nil || e.Code == "" {
			e = errorResponse{Code: codeInternal, Message: resp.Status}
		}
		return &Error{Status: resp.StatusCode, Code: e.Code, Msg: e.Message}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// LastContact is when the control plane last answered.
func (c *Client) LastContact() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastOK
}

// Run keeps Changed() working and reports the node alive until ctx ends.
func (c *Client) Run(ctx context.Context) {
	go c.heartbeats(ctx)
	delay := time.Second
	for ctx.Err() == nil {
		c.mu.Lock()
		after := c.rev
		c.mu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 65*time.Second)
		var resp waitResponse
		err := c.do(wctx, http.MethodGet, "wait", url.Values{"after": {strconv.FormatInt(after, 10)}, "timeout": {"50"}}, nil, &resp)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.logf("waiting for changes: %v (again in %s)", err, delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			delay = min(2*delay, 10*time.Second)
			continue
		}
		delay = time.Second
		if resp.Rev != after {
			c.mu.Lock()
			c.rev = resp.Rev
			close(c.wake)
			c.wake = make(chan struct{})
			c.mu.Unlock()
		}
	}
}

func (c *Client) heartbeats(ctx context.Context) {
	t := time.NewTicker(HeartbeatEvery)
	defer t.Stop()
	for {
		if err := c.do(ctx, http.MethodPost, "heartbeat", nil, nil, nil); err != nil && ctx.Err() == nil {
			c.logf("heartbeat: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- store.Reader ----

func (c *Client) Changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wake
}

func (c *Client) Get(k store.Key) (store.Record, error) {
	var resp listResponse
	q := url.Values{"group": {k.Group}, "resource": {k.Resource}, "namespace": {k.Namespace}, "name": {k.Name}}
	if err := c.do(context.Background(), http.MethodGet, "objects", q, nil, &resp); err != nil {
		return store.Record{}, err
	}
	if len(resp.Items) != 1 {
		return store.Record{}, store.ErrNotFound
	}
	return resp.Items[0].record(), nil
}

func (c *Client) List(group, resource, namespace string) ([]store.Record, int64, error) {
	var resp listResponse
	q := url.Values{"group": {group}, "resource": {resource}, "namespace": {namespace}}
	if err := c.do(context.Background(), http.MethodGet, "objects", q, nil, &resp); err != nil {
		return nil, 0, err
	}
	out := make([]store.Record, len(resp.Items))
	for i, r := range resp.Items {
		out[i] = r.record()
	}
	return out, resp.Rev, nil
}

// ---- store.Backend ----

func (c *Client) Changes(group, resource, namespace string, after int64, limit int) ([]store.Change, int64, error) {
	var resp changesResponse
	q := url.Values{"group": {group}, "resource": {resource}, "namespace": {namespace},
		"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(limit)}}
	if err := c.do(context.Background(), http.MethodGet, "changes", q, nil, &resp); err != nil {
		return nil, 0, err
	}
	out := make([]store.Change, len(resp.Changes))
	for i, ch := range resp.Changes {
		out[i] = store.Change{Rev: ch.Rev, Key: ch.Key.key(), Type: ch.Type, Data: []byte(ch.Data)}
		if len(ch.Prev) > 0 {
			out[i].Prev = []byte(ch.Prev)
		}
	}
	return out, resp.Next, nil
}

func (c *Client) AllocIP(kind, uid string, candidates func(yield func(string) bool)) (string, error) {
	var list []string
	for ip := range candidates {
		list = append(list, ip)
		if len(list) == 1024 {
			break
		}
	}
	var resp allocResponse
	err := c.do(context.Background(), http.MethodPost, "ips/alloc", nil, allocRequest{Kind: kind, UID: uid, Candidates: list}, &resp)
	return resp.IP, err
}

func (c *Client) ReleaseIP(uid string) error {
	return c.do(context.Background(), http.MethodPost, "ips/release", nil, releaseRequest{UID: uid}, nil)
}

// IPs returns the allocations of this node's pod range only.
func (c *Client) IPs(kind string) (map[string]string, error) {
	out := map[string]string{}
	err := c.do(context.Background(), http.MethodGet, "ips", url.Values{"kind": {kind}}, nil, &out)
	return out, err
}

// maxAttempts bounds how often Transact repeats fn after conflicts.
const maxAttempts = 8

// Transact runs fn as an optimistic transaction: the control plane applies
// the writes only if every object fn read is unchanged; otherwise fn runs
// again on fresh reads.
func (c *Client) Transact(fn func(store.Txn) error) error {
	for attempt := 1; ; attempt++ {
		t := &remoteTxn{c: c, reads: map[store.Key]txRead{}, view: map[store.Key]*store.Record{}, pending: map[store.Key]int{}}
		if err := fn(t); err != nil {
			return err
		}
		if len(t.writes) == 0 {
			return nil
		}
		req := txRequest{Writes: t.writes}
		for _, r := range t.reads {
			req.Reads = append(req.Reads, r)
		}
		err := c.do(context.Background(), http.MethodPost, "transact", nil, req, nil)
		var e *Error
		if errors.As(err, &e) && e.Code == codeConflict && attempt < maxAttempts {
			time.Sleep(time.Duration(attempt*attempt) * 20 * time.Millisecond)
			continue
		}
		return err
	}
}

type remoteTxn struct {
	c       *Client
	reads   map[store.Key]txRead
	writes  []txWrite
	pending map[store.Key]int           // key -> index in writes
	view    map[store.Key]*store.Record // what this transaction wrote; nil: deleted
}

func (t *remoteTxn) Get(k store.Key) (store.Record, error) {
	if rec, ok := t.view[k]; ok {
		if rec == nil {
			return store.Record{}, store.ErrNotFound
		}
		return *rec, nil
	}
	rec, err := t.c.Get(k)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if _, seen := t.reads[k]; !seen {
			t.reads[k] = txRead{Key: toWireKey(k), Exists: false}
		}
		return store.Record{}, err
	case err != nil:
		return store.Record{}, err
	}
	if _, seen := t.reads[k]; !seen {
		t.reads[k] = txRead{Key: toWireKey(k), Exists: true, Rev: rec.Rev}
	}
	return rec, nil
}

func (t *remoteTxn) write(op string, k store.Key, expectRev int64, data []byte) {
	w := txWrite{Op: op, Key: toWireKey(k), ExpectRev: expectRev, Data: data}
	if i, ok := t.pending[k]; ok {
		// A second write to the same object replaces the first; it keeps
		// the first one's operation (create stays create) and expectation.
		prev := t.writes[i]
		if prev.Op == "create" && op == "update" {
			w.Op = "create"
		}
		w.ExpectRev = prev.ExpectRev
		t.writes[i] = w
		return
	}
	t.pending[k] = len(t.writes)
	t.writes = append(t.writes, w)
}

func (t *remoteTxn) Create(p store.Put) (store.Record, error) {
	if rec, ok := t.view[p.Key]; ok && rec != nil {
		return store.Record{}, store.ErrExists
	}
	data, err := p.Encode(0)
	if err != nil {
		return store.Record{}, err
	}
	t.write("create", p.Key, 0, data)
	rec := store.Record{Key: p.Key, UID: p.UID, Generation: p.Generation, Deleting: p.Deleting, Data: data}
	t.view[p.Key] = &rec
	return rec, nil
}

func (t *remoteTxn) Update(p store.Put, expectRev int64) (store.Record, bool, error) {
	data, err := p.Encode(expectRev)
	if err != nil {
		return store.Record{}, false, err
	}
	t.write("update", p.Key, expectRev, data)
	rec := store.Record{Key: p.Key, UID: p.UID, Rev: expectRev, Generation: p.Generation, Deleting: p.Deleting, Data: data}
	t.view[p.Key] = &rec
	return rec, true, nil
}

func (t *remoteTxn) Delete(k store.Key, expectRev int64, encode func(int64) ([]byte, error)) (store.Record, error) {
	data, err := encode(expectRev)
	if err != nil {
		return store.Record{}, err
	}
	t.write("delete", k, expectRev, data)
	t.view[k] = nil
	return store.Record{Key: k, Rev: expectRev, Data: data}, nil
}

// ---- store.NetworkReader ----

func (c *Client) ServiceNetwork(ctx context.Context) (store.NetworkView, error) {
	var resp networkResponse
	if err := c.do(ctx, http.MethodGet, "network", nil, nil, &resp); err != nil {
		return store.NetworkView{}, err
	}
	conv := func(in []wireRecord) []store.Record {
		out := make([]store.Record, len(in))
		for i, r := range in {
			out[i] = r.record()
		}
		return out
	}
	return store.NetworkView{Services: conv(resp.Services), EndpointSlices: conv(resp.EndpointSlices),
		Pods: conv(resp.Pods), PodIPs: resp.PodIPs}, nil
}

func (c *Client) PodAtIP(ctx context.Context, namespace, name, ip string) (store.Record, error) {
	var resp wireRecord
	err := c.do(ctx, http.MethodGet, "podatip", url.Values{"namespace": {namespace}, "name": {name}, "ip": {ip}}, nil, &resp)
	return resp.record(), err
}

// ---- joining ----

// FetchCA downloads the cluster CA without trusting the server yet and checks
// it against the hash the administrator handed out with the join token.
func FetchCA(ctx context.Context, baseURL, wantHash string) ([]byte, error) {
	insecure := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}} // checked by hash below
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+Prefix+"ca", nil)
	if err != nil {
		return nil, err
	}
	resp, err := insecure.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	ca, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the control plane answered %s for its CA (is it a Shearwater control plane?)", resp.Status)
	}
	got, err := pki.CAHash(ca)
	if err != nil {
		return nil, err
	}
	if got != wantHash {
		return nil, fmt.Errorf("the control plane's CA hash is %s, not %s: do not join (wrong address, or someone in between)", got, wantHash)
	}
	return ca, nil
}

// Join registers this server with the control plane.
func Join(ctx context.Context, baseURL, joinToken string, caPEM []byte, req JoinRequest) (JoinResponse, error) {
	c, err := NewClient(baseURL, joinToken, caPEM, netip.Prefix{})
	if err != nil {
		return JoinResponse{}, err
	}
	var resp JoinResponse
	err = c.do(ctx, http.MethodPost, "join", nil, req, &resp)
	return resp, err
}
