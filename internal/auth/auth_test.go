package auth

import (
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"shearwater/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.InitCredentials(); err != nil {
		t.Fatal(err)
	}
	return st
}

func issue(t *testing.T, st *store.Store, kind, name string, groups []string, expires time.Time) string {
	t.Helper()
	tok, id, hash, err := NewToken(kind)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutCredential(store.Credential{ID: id, Kind: kind, Name: name, Groups: groups, Hash: hash, Expires: expires}); err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestTokensIdentifyTheirHolder(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	a := &Authenticator{AdminToken: "admin-secret", Credentials: st, Now: func() time.Time { return now }}

	if id, ok := a.Token("admin-secret"); !ok || !id.IsAdmin() || id.Kind != KindAdmin {
		t.Fatalf("admin token: %+v %v", id, ok)
	}
	// A stored user credential cannot claim system:masters.
	alice := issue(t, st, KindUser, "alice", []string{"team-a", GroupMasters}, time.Time{})
	id, ok := a.Token(alice)
	if !ok || id.Name != "alice" || id.Kind != KindUser || id.IsAdmin() {
		t.Fatalf("user token: %+v %v", id, ok)
	}
	if !slices.Equal(id.Groups, []string{GroupAuthenticated, "team-a"}) {
		t.Fatalf("user groups: %v", id.Groups)
	}
	node := issue(t, st, KindNode, "worker-1", nil, time.Time{})
	id, ok = a.Token(node)
	if name, isNode := id.NodeName(); !ok || !isNode || name != "worker-1" || !slices.Contains(id.Groups, GroupNodes) {
		t.Fatalf("node token: %+v %v", id, ok)
	}
	expired := issue(t, st, KindJoin, "join", nil, now)
	if _, ok := a.Token(expired); ok {
		t.Fatal("a token at its expiry time must not work")
	}

	_, pub, secret, _ := Parse(alice)
	for name, tok := range map[string]string{
		"wrong secret":       "swu_" + pub + "_" + secret + "x",
		"wrong kind letter":  "swn_" + pub + "_" + secret,
		"unknown id":         "swu_zzzzzzzz_" + secret,
		"not a token":        "Bearer nonsense",
		"empty":              "",
		"admin token prefix": "admin-secre",
	} {
		if _, ok := a.Token(tok); ok {
			t.Errorf("%s: accepted", name)
		}
	}

	r, _ := http.NewRequest("GET", "/api", nil)
	r.Header.Set("Authorization", "Bearer "+alice)
	if id, ok := a.Authenticate(r); !ok || id.Name != "alice" {
		t.Fatalf("Authenticate: %+v %v", id, ok)
	}
	if n, err := st.DeleteCredentials(KindUser, "alice"); err != nil || n != 1 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if _, ok := a.Token(alice); ok {
		t.Fatal("a removed user's token still works")
	}
}

func TestParseRoundTrip(t *testing.T) {
	for _, kind := range []string{KindUser, KindNode, KindJoin} {
		tok, id, hash, err := NewToken(kind)
		if err != nil {
			t.Fatal(err)
		}
		k, pid, secret, ok := Parse(tok)
		if !ok || k != kind || pid != id || Hash(secret) != hash {
			t.Fatalf("%s: %q -> %v %q %q", kind, tok, ok, k, pid)
		}
	}
	if _, _, _, err := NewToken(KindAdmin); err == nil {
		t.Fatal("administrator tokens are not issued here")
	}
}
