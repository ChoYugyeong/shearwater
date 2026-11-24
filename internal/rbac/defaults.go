package rbac

import (
	"errors"
	"slices"

	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

func adminAll() []apis.PolicyRule {
	return []apis.PolicyRule{
		{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}},
		{Verbs: []string{"*"}, NonResourceURLs: []string{"*"}},
	}
}

func defaults() map[string][]apis.PolicyRule {
	read := []string{"get", "list", "watch"}
	write := []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
	rule := func(verbs []string, group string, resources ...string) apis.PolicyRule {
		return apis.PolicyRule{Verbs: verbs, APIGroups: []string{group}, Resources: resources}
	}
	view := []apis.PolicyRule{
		rule(read, "", "pods", "pods/log", "pods/status", "services", "events", "persistentvolumeclaims", "resourcequotas", "resourcequotas/status"),
		rule(read, "apps", "deployments", "deployments/scale", "deployments/status", "replicasets", "replicasets/scale", "replicasets/status"),
		rule(read, "networking.k8s.io", "ingresses", "ingresses/status", "networkpolicies"),
		rule(read, "discovery.k8s.io", "endpointslices"),
	}
	edit := append(slices.Clone(view),
		rule(write, "", "pods", "services", "secrets", "persistentvolumeclaims"),
		rule(write, "apps", "deployments", "deployments/scale", "replicasets", "replicasets/scale"),
		rule(write, "networking.k8s.io", "ingresses", "networkpolicies"),
	)
	admin := append(slices.Clone(edit), rule(write, apis.RBACGroup, "roles", "rolebindings"))
	return map[string][]apis.PolicyRule{"cluster-admin": adminAll(), "admin": admin, "edit": edit, "view": view}
}

// Bootstrap installs missing default roles while preserving administrator edits.
func Bootstrap(s *store.Store) error {
	res := apis.Lookup(apis.RBACGroup, "v1", "clusterroles")
	return s.Write(false, func(tx *store.Tx) error {
		for name, rules := range defaults() {
			role := &apis.Role{Metadata: apis.ObjectMeta{Name: name, Labels: map[string]string{"kubernetes.io/bootstrapping": "rbac-defaults"}}, Rules: rules}
			if _, err := tx.Get(objects.Key(res, "", name)); err == nil {
				continue
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
			objects.Stamp(res, role)
			if _, err := tx.Create(objects.Put(res, role)); err != nil {
				return err
			}
		}
		return nil
	})
}
