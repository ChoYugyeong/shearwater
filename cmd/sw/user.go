package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"shearwater/internal/api"
	"shearwater/internal/apis"
	"shearwater/internal/auth"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

// adminActor attributes the namespaces and bindings sw user and sw agent
// create to the administrator in sw changes.
var adminActor = store.Actor{Name: auth.AdminName, Via: "admin"}

const (
	usersPath     = api.AdminPrefix + "v1/users"
	userLabel     = "shearwater.io/user"
	agentLabel    = "shearwater.io/agent"
	securityLabel = "pod-security.kubernetes.io/enforce"
)

type userOptions struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace,omitempty"`
	Role        string `json:"role,omitempty"`
	ClusterRole string `json:"clusterRole,omitempty"`
	Agent       bool   `json:"agent,omitempty"`
	Created     string `json:"created,omitempty"`
}

type issuedUser struct {
	userOptions
	Token string `json:"token"`
}

type userManager struct {
	st *store.Store
	mu sync.Mutex
}

func registerUsers(w *wiring) {
	users := &userManager{st: w.Store}
	w.Admin.Handle(usersPath, users)
	w.Admin.Handle(usersPath+"/", users)
}

type userFailure struct {
	status  int
	message string
}

func (e *userFailure) Error() string { return e.message }

func badUser(format string, args ...any) error {
	return &userFailure{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

func userError(err error) apis.Status {
	code, reason, hint := "INTERNAL", "InternalError", "check the server logs and list the users before retrying"
	status := http.StatusInternalServerError
	var failure *userFailure
	switch {
	case errors.As(err, &failure):
		status = failure.status
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, store.ErrExists), errors.Is(err, store.ErrConflict):
		status = http.StatusConflict
	}
	switch status {
	case 400:
		code, reason, hint = "INVALID", "BadRequest", "check the user, namespace and requested role"
	case 404:
		code, reason, hint = "NOT_FOUND", "NotFound", "check the user name or referenced ClusterRole"
	case 405:
		code, reason, hint = "UNSUPPORTED", "MethodNotAllowed", "use the method for this user endpoint"
	case 409:
		code, reason, hint = "CONFLICT", "Conflict", "inspect the existing user, namespace and bindings"
	}
	return apis.Status{
		TypeMeta: apis.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   "Failure", Code: status, Reason: reason,
		Message: fmt.Sprintf("%s retry=never: %s\nhint: %s", code, err, hint),
	}
}

func (u *userManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	u.mu.Lock()
	result, status, err := u.handle(w, r)
	u.mu.Unlock()
	if err != nil {
		failure := userError(err)
		result, status = failure, failure.Code
	}
	w.WriteHeader(status)
	if result != nil {
		_ = json.NewEncoder(w).Encode(result)
	}
}

func (u *userManager) handle(w http.ResponseWriter, r *http.Request) (any, int, error) {
	if r.URL.Path == usersPath {
		switch r.Method {
		case http.MethodGet:
			items, err := u.list()
			return map[string]any{"items": items}, 200, err
		case http.MethodPost:
			var o userOptions
			d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
			d.DisallowUnknownFields()
			if err := d.Decode(&o); err != nil {
				return nil, 0, badUser("invalid user: %v", err)
			}
			if d.Decode(new(any)) != io.EOF {
				return nil, 0, badUser("expected one user object")
			}
			issued, err := u.add(o)
			return issued, 201, err
		default:
			return nil, 0, &userFailure{405, "unsupported users method"}
		}
	}
	name, sub, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, usersPath+"/"), "/")
	if !validUserName(name) || (sub != "" && sub != "tokens") {
		return nil, 0, store.ErrNotFound
	}
	c, err := u.st.Credential(userID(name))
	if err != nil {
		return nil, 0, err
	}
	if c.Kind != auth.KindUser || c.Name != name {
		return nil, 0, store.ErrNotFound
	}
	switch {
	case r.Method == http.MethodPost && sub == "tokens":
		if c.Hash == "" {
			return nil, 0, &userFailure{409, "user is inactive; finish removing it before adding it again"}
		}
		var o userOptions
		if err := json.Unmarshal(c.Data, &o); err != nil {
			return nil, 0, err
		}
		issued, err := u.issue(c, o)
		return issued, 201, err
	case r.Method == http.MethodDelete && sub == "":
		return nil, 204, u.remove(c)
	default:
		return nil, 0, &userFailure{405, "unsupported user method"}
	}
}

func (u *userManager) list() ([]userOptions, error) {
	creds, err := u.st.Credentials(auth.KindUser)
	if err != nil {
		return nil, err
	}
	items := make([]userOptions, 0, len(creds))
	for _, c := range creds {
		o := userOptions{Name: c.Name}
		if len(c.Data) > 0 {
			if err := json.Unmarshal(c.Data, &o); err != nil {
				return nil, err
			}
		}
		if !c.Created.IsZero() {
			o.Created = c.Created.UTC().Format(time.RFC3339)
		}
		items = append(items, o)
	}
	slices.SortFunc(items, func(a, b userOptions) int { return strings.Compare(a.Name, b.Name) })
	return items, nil
}

// The public id doubles as an indexed user-name lookup. Only the random
// secret authenticates requests, and rotation replaces its hash in one write.
func userID(name string) string { return "user-" + name }

func validUserName(name string) bool {
	return apis.IsDNS1123Label(name) && name != auth.AdminName
}

func (u *userManager) add(o userOptions) (issuedUser, error) {
	if !validUserName(o.Name) {
		return issuedUser{}, badUser("invalid or reserved user name %q", o.Name)
	}
	if o.Created != "" {
		return issuedUser{}, badUser("created is server-managed")
	}
	if o.Agent && o.Namespace == "" {
		return issuedUser{}, badUser("agent requires a namespace")
	}
	if o.Namespace != "" && !apis.IsDNS1123Label(o.Namespace) {
		return issuedUser{}, badUser("invalid namespace %q", o.Namespace)
	}
	if o.Namespace != "" && o.Role == "" {
		o.Role = "edit"
	}
	if o.Role != "" && (o.Namespace == "" || !slices.Contains([]string{"view", "edit", "admin"}, o.Role)) {
		return issuedUser{}, badUser("role must be admin, edit or view and requires a namespace")
	}
	if o.ClusterRole == "." || o.ClusterRole == ".." || strings.ContainsAny(o.ClusterRole, "/%") {
		return issuedUser{}, badUser("invalid cluster role name")
	}
	id := userID(o.Name)
	if _, err := u.st.Credential(id); !errors.Is(err, store.ErrNotFound) {
		if err == nil {
			err = fmt.Errorf("user %q: %w", o.Name, store.ErrExists)
		}
		return issuedUser{}, err
	}
	data, err := json.Marshal(o)
	if err != nil {
		return issuedUser{}, err
	}
	c := store.Credential{ID: id, Kind: auth.KindUser, Name: o.Name, Created: time.Now(), Data: data}
	// Reserve an inactive identity until every grant has committed.
	if err := u.st.PutCredential(c); err != nil {
		return issuedUser{}, err
	}
	err = u.st.WriteAs(adminActor, false, func(tx *store.Tx) error {
		if o.Namespace != "" {
			if err := ensureTenantNamespace(tx, o); err != nil {
				return err
			}
			if err := createUserBinding(tx, o.Name, o.Namespace, o.Role, o.Agent); err != nil {
				return err
			}
		}
		if o.ClusterRole != "" {
			return createUserBinding(tx, o.Name, "", o.ClusterRole, o.Agent)
		}
		return nil
	})
	if err != nil {
		return issuedUser{}, errors.Join(err, u.st.DeleteCredential(id))
	}
	return u.issue(c, o)
}

func (u *userManager) issue(c store.Credential, o userOptions) (issuedUser, error) {
	token, _, hash, err := auth.NewToken(auth.KindUser)
	if err != nil {
		return issuedUser{}, err
	}
	_, _, secret, _ := auth.Parse(token)
	c.Hash = hash
	if err := u.st.PutCredential(c); err != nil {
		return issuedUser{}, err
	}
	if !c.Created.IsZero() {
		o.Created = c.Created.UTC().Format(time.RFC3339)
	}
	return issuedUser{o, "swu_" + c.ID + "_" + secret}, nil
}

func ensureTenantNamespace(tx *store.Tx, o userOptions) error {
	res := apis.Lookup("", "v1", "namespaces")
	rec, err := tx.Get(objects.Key(res, "", o.Namespace))
	ns := &apis.Namespace{}
	if errors.Is(err, store.ErrNotFound) {
		ns.Metadata.Name = o.Namespace
		ns.Metadata.Labels = map[string]string{"shearwater.io/tenant": o.Name, securityLabel: "baseline"}
		if o.Agent {
			ns.Metadata.Labels[agentLabel] = o.Name
		}
		objects.Stamp(res, ns)
		_, err := tx.Create(objects.Put(res, ns))
		return err
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(rec.Data, ns); err != nil {
		return err
	}
	if rec.Deleting {
		return &userFailure{409, "namespace is terminating"}
	}
	switch ns.Metadata.Labels[securityLabel] {
	case "baseline", "restricted":
		return nil
	case "":
		if ns.Metadata.Labels == nil {
			ns.Metadata.Labels = map[string]string{}
		}
		ns.Metadata.Labels[securityLabel] = "baseline"
		_, _, err := tx.Update(objects.Put(res, ns), rec.Rev)
		return err
	default:
		return &userFailure{409, "namespace must enforce baseline or restricted Pod Security"}
	}
}

func createUserBinding(tx *store.Tx, name, ns, role string, agent bool) error {
	if _, err := tx.Get(store.Key{Group: apis.RBACGroup, Resource: "clusterroles", Name: role}); err != nil {
		return fmt.Errorf("clusterrole %q: %w", role, err)
	}
	ref := apis.RoleRef{APIGroup: apis.RBACGroup, Kind: "ClusterRole", Name: role}
	subjects := []apis.Subject{{APIGroup: apis.RBACGroup, Kind: "User", Name: name}}
	var obj apis.Object = &apis.RoleBinding{RoleRef: ref, Subjects: subjects}
	resource := "rolebindings"
	if ns == "" {
		obj = &apis.ClusterRoleBinding{RoleRef: ref, Subjects: subjects}
		resource = "clusterrolebindings"
	}
	res := apis.Lookup(apis.RBACGroup, "v1", resource)
	labels := map[string]string{userLabel: name}
	if agent {
		labels[agentLabel] = name
	}
	*obj.Meta() = apis.ObjectMeta{Name: "sw-user." + name, Namespace: ns, Labels: labels}
	objects.Stamp(res, obj)
	_, err := tx.Create(objects.Put(res, obj))
	return err
}

func (u *userManager) remove(c store.Credential) error {
	// Revoke first. Keep the inactive row until cleanup commits so a retry
	// after a failure can finish and the old name cannot be reissued early.
	c.Hash = ""
	if err := u.st.PutCredential(c); err != nil {
		return err
	}
	var bindings []store.Record
	for _, resource := range []string{"rolebindings", "clusterrolebindings"} {
		records, _, err := u.st.List(apis.RBACGroup, resource, "")
		if err != nil {
			return err
		}
		bindings = append(bindings, records...)
	}
	err := u.st.WriteAs(adminActor, false, func(tx *store.Tx) error {
		for _, candidate := range bindings {
			rec, err := tx.Get(candidate.Key)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			res := apis.Lookup(apis.RBACGroup, "v1", rec.Key.Resource)
			obj := res.New()
			if err := json.Unmarshal(rec.Data, obj); err != nil {
				return err
			}
			if obj.Meta().Labels[userLabel] == c.Name || obj.Meta().Labels[agentLabel] == c.Name {
				if _, err := tx.Delete(rec.Key, rec.Rev, objects.Encoder(obj)); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = u.st.DeleteCredentials(auth.KindUser, c.Name)
	return err
}
