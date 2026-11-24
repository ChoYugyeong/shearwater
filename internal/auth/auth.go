// Package auth says who sent a request and carries the request attributes an
// authorizer judges. It holds no policy: the RBAC authorizer and the node API
// decide what an identity may do (docs/design-multinode-tenancy.md 2).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"shearwater/internal/store"
)

// Identity kinds.
const (
	KindAdmin = "admin" // the local administrator token (admin.token)
	KindUser  = "user"  // a person or an AI agent added with `sw user add`
	KindNode  = "node"  // a worker node's own credential, issued at join
	KindJoin  = "join"  // a short-lived token that lets one server join
)

// Well-known groups. Their names follow Kubernetes so RBAC objects written
// for Kubernetes keep their meaning here.
const (
	GroupMasters       = "system:masters"
	GroupAuthenticated = "system:authenticated"
	GroupNodes         = "system:nodes"
	AdminName          = "shearwater-admin"
	NodePrefix         = "system:node:"
)

// Identity is who sent a request.
type Identity struct {
	Name   string   // AdminName, a user name, or NodePrefix+node name
	Groups []string // always includes GroupAuthenticated
	Kind   string   // KindAdmin, KindUser, KindNode or KindJoin
	// CredentialID is the public part of the token that proved it ("" for
	// the administrator token).
	CredentialID string
}

// IsAdmin reports whether the identity bypasses authorization.
func (id Identity) IsAdmin() bool { return slices.Contains(id.Groups, GroupMasters) }

// NodeName returns the node a node identity speaks for.
func (id Identity) NodeName() (string, bool) {
	if id.Kind != KindNode {
		return "", false
	}
	return strings.CutPrefix(id.Name, NodePrefix)
}

// Admin is the identity of the administrator token.
func Admin() Identity {
	return Identity{Name: AdminName, Kind: KindAdmin, Groups: []string{GroupMasters, GroupAuthenticated}}
}

type ctxKey struct{}

// WithIdentity returns a context that carries id.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// From returns the identity a context carries.
func From(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// ---- tokens ----

// Tokens look like sw<k>_<id>_<secret>: k names the kind (u user, n node,
// j join), id is public and indexes the stored credential, and only the SHA-256
// of secret is stored. A leaked database therefore does not leak usable tokens.
var kindLetters = map[string]string{KindUser: "u", KindNode: "n", KindJoin: "j"}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return strings.ToLower(b32.EncodeToString(b))
}

// NewToken returns a fresh token of the given kind, its public id and the hash
// to store.
func NewToken(kind string) (token, id, hash string, err error) {
	letter, ok := kindLetters[kind]
	if !ok {
		return "", "", "", errors.New("auth: no tokens of kind " + kind)
	}
	id, secret := random(5), random(20)
	return "sw" + letter + "_" + id + "_" + secret, id, Hash(secret), nil
}

// Parse splits a token. It does not check it against anything.
func Parse(token string) (kind, id, secret string, ok bool) {
	parts := strings.Split(token, "_")
	if len(parts) != 3 || len(parts[0]) != 3 || !strings.HasPrefix(parts[0], "sw") || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	for k, letter := range kindLetters {
		if parts[0][2:] == letter {
			return k, parts[1], parts[2], true
		}
	}
	return "", "", "", false
}

// Hash is the stored form of a token secret.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ---- authentication ----

// Credentials is where issued tokens are looked up (*store.Store).
type Credentials interface {
	Credential(id string) (store.Credential, error)
}

// Authenticator turns a bearer token into an identity.
type Authenticator struct {
	AdminToken  string
	Credentials Credentials // nil: only the administrator token works
	Now         func() time.Time
}

// Bearer returns the bearer token of a request.
func Bearer(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok, ok && tok != ""
}

// Authenticate identifies the sender of r.
func (a *Authenticator) Authenticate(r *http.Request) (Identity, bool) {
	tok, ok := Bearer(r)
	if !ok {
		return Identity{}, false
	}
	return a.Token(tok)
}

// Token identifies the holder of a bearer token. Join tokens are recognized
// too; only the node API accepts them.
func (a *Authenticator) Token(tok string) (Identity, bool) {
	if a.AdminToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.AdminToken)) == 1 {
		return Admin(), true
	}
	kind, id, secret, ok := Parse(tok)
	if !ok || a.Credentials == nil {
		return Identity{}, false
	}
	c, err := a.Credentials.Credential(id)
	if err != nil || c.Kind != kind {
		return Identity{}, false
	}
	if subtle.ConstantTimeCompare([]byte(Hash(secret)), []byte(c.Hash)) != 1 {
		return Identity{}, false
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	if !c.Expires.IsZero() && !now().Before(c.Expires) {
		return Identity{}, false
	}
	groups := append(slices.Clone(c.Groups), GroupAuthenticated)
	name := c.Name
	switch kind {
	case KindNode:
		name = NodePrefix + c.Name
		groups = append(groups, GroupNodes)
	case KindUser:
		// A stored user credential can never claim system:masters; the
		// administrator token is the only way to bypass authorization.
		groups = slices.DeleteFunc(groups, func(g string) bool { return g == GroupMasters })
	}
	slices.Sort(groups)
	return Identity{Name: name, Groups: slices.Compact(groups), Kind: kind, CredentialID: id}, true
}

// ---- authorization ----

// Attributes describe one request in the terms RBAC rules use.
type Attributes struct {
	ResourceRequest bool
	// Resource requests.
	Verb        string // get, list, watch, create, update, patch, delete, deletecollection
	APIGroup    string // "" for the core group
	APIVersion  string
	Resource    string // plural, for example "pods"
	Subresource string // "status", "scale", "log", ...
	Namespace   string // "" for cluster-scoped resources and cluster-wide lists
	Name        string // "" for collections
	// Non-resource requests (/api, /apis, /version, /openapi/...).
	Path string
}

// Authorizer decides whether an identity may make a request. The API server
// never asks it about administrators.
type Authorizer interface {
	Authorize(id Identity, a Attributes) (allowed bool, reason string)
}
