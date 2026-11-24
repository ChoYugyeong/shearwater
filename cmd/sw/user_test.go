package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"shearwater/internal/api"
	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func TestUserAdminLifecycle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.InitCredentials(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerUsers(&wiring{Store: st, Admin: mux})
	authn := &auth.Authenticator{AdminToken: "admin", Credentials: st}
	srv := &api.Server{Store: st, Admin: mux, Auth: authn}
	request := func(token, method, path, body string, want int) string {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d, want %d", method, path, w.Code, want)
		}
		return w.Body.String()
	}
	decode := func(body string) issuedUser {
		t.Helper()
		var issued issuedUser
		if err := json.Unmarshal([]byte(body), &issued); err != nil {
			t.Fatal(err)
		}
		return issued
	}
	const add = `{"name":"alice","namespace":"team-a","role":"view"}`
	request("admin", "POST", usersPath, add, 404)
	if _, err := st.Get(store.Key{Resource: "namespaces", Name: "team-a"}); err != store.ErrNotFound {
		t.Fatalf("failed add left namespace: %v", err)
	}
	err = st.Write(false, func(tx *store.Tx) error {
		res := apis.Lookup(apis.RBACGroup, "v1", "clusterroles")
		role := &apis.ClusterRole{Metadata: apis.ObjectMeta{Name: "view"}}
		objects.Stamp(res, role)
		_, err := tx.Create(objects.Put(res, role))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	user := decode(request("admin", "POST", usersPath, add, 201))
	if id, ok := authn.Token(user.Token); !ok || id.Name != "alice" || id.IsAdmin() {
		t.Fatal("issued token did not authenticate as alice")
	}
	request(user.Token, "GET", usersPath, "", 403)
	request("admin", "POST", usersPath, add, 409)
	list := request("admin", "GET", usersPath, "", 200)
	if !strings.Contains(list, `"name":"alice"`) || strings.Contains(list, "token") || strings.Contains(list, "hash") {
		t.Fatal("unexpected user list")
	}
	config := string(userKubeconfig("https://example", []byte("test-ca"), user))
	if !strings.Contains(config, user.Token) || !strings.Contains(config, `namespace: "team-a"`) || !strings.Contains(config, `user: "alice"`) {
		t.Fatal("unexpected kubeconfig")
	}
	nsKey := store.Key{Resource: "namespaces", Name: "team-a"}
	ns, err := st.Get(nsKey)
	if err != nil {
		t.Fatal(err)
	}
	var namespace apis.Namespace
	if err := json.Unmarshal(ns.Data, &namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Metadata.Labels[securityLabel] != "baseline" || namespace.Metadata.Labels["shearwater.io/tenant"] != "alice" {
		t.Fatal("missing tenant namespace labels")
	}
	key := store.Key{Group: apis.RBACGroup, Resource: "rolebindings", Namespace: "team-a", Name: "sw-user.alice"}
	rec, err := st.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	var binding apis.RoleBinding
	if err := json.Unmarshal(rec.Data, &binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef.Name != "view" || len(binding.Subjects) != 1 || binding.Subjects[0].Name != "alice" || binding.Metadata.Labels[userLabel] != "alice" {
		t.Fatal("unexpected tenant binding")
	}
	rotated := decode(request("admin", "POST", usersPath+"/alice/tokens", "", 201))
	if _, ok := authn.Token(user.Token); ok {
		t.Fatal("old token survived rotation")
	}
	if _, ok := authn.Token(rotated.Token); !ok {
		t.Fatal("rotated token rejected")
	}
	request("admin", "DELETE", usersPath+"/alice", "", 204)
	if _, ok := authn.Token(rotated.Token); ok {
		t.Fatal("removed user token still active")
	}
	if _, err := st.Get(key); err != store.ErrNotFound {
		t.Fatalf("removed user binding: %v", err)
	}
	if _, err := st.Get(nsKey); err != nil {
		t.Fatalf("tenant namespace should survive removal: %v", err)
	}
}
