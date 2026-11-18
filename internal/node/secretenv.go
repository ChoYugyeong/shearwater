package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

// secretEnv resolves a container's envFrom and env entries that read Secrets.
// It returns the variables in Kubernetes precedence: envFrom sources in order
// (a later source wins), then env entries (which win over envFrom). Values
// are read when the container is created, so a changed Secret reaches a
// container only when it is created again, as in Kubernetes.
//
// A missing Secret or key fails the start with CreateContainerConfigError
// unless the reference is optional. Error texts name the Secret and key but
// never contain a value.
func (a *Agent) secretEnv(namespace string, c *apis.Container) ([]apis.EnvVar, error) {
	cache := map[string]*apis.Secret{}
	load := func(name string) (*apis.Secret, error) {
		if s, ok := cache[name]; ok {
			return s, nil
		}
		rec, err := a.Store.Get(store.Key{Resource: "secrets", Namespace: namespace, Name: name})
		if errors.Is(err, store.ErrNotFound) || err == nil && rec.Deleting {
			cache[name] = nil
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading secret %q: %w", name, err)
		}
		var s apis.Secret
		if err := json.Unmarshal(rec.Data, &s); err != nil {
			return nil, fmt.Errorf("decoding secret %q: %w", name, err)
		}
		cache[name] = &s
		return &s, nil
	}
	optional := func(p *bool) bool { return p != nil && *p }

	var out []apis.EnvVar
	index := map[string]int{}
	set := func(name, value string) error {
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("variable %s would contain a NUL byte", name)
		}
		if i, ok := index[name]; ok {
			out[i].Value = value
			return nil
		}
		index[name] = len(out)
		out = append(out, apis.EnvVar{Name: name, Value: value})
		return nil
	}
	for _, from := range c.EnvFrom {
		if from.SecretRef == nil {
			continue
		}
		s, err := load(from.SecretRef.Name)
		if err != nil {
			return nil, err
		}
		if s == nil {
			if optional(from.SecretRef.Optional) {
				continue
			}
			return nil, fmt.Errorf("secret %q not found", from.SecretRef.Name)
		}
		for _, k := range slices.Sorted(maps.Keys(s.Data)) {
			if err := set(from.Prefix+k, string(s.Data[k])); err != nil {
				return nil, err
			}
		}
	}
	for _, e := range c.Env {
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			if err := set(e.Name, e.Value); err != nil {
				return nil, err
			}
			continue
		}
		ref := e.ValueFrom.SecretKeyRef
		s, err := load(ref.Name)
		if err != nil {
			return nil, err
		}
		if s == nil {
			if optional(ref.Optional) {
				continue
			}
			return nil, fmt.Errorf("secret %q not found", ref.Name)
		}
		v, ok := s.Data[ref.Key]
		if !ok {
			if optional(ref.Optional) {
				continue
			}
			return nil, fmt.Errorf("couldn't find key %s in Secret %s/%s", ref.Key, namespace, ref.Name)
		}
		if err := set(e.Name, string(v)); err != nil {
			return nil, err
		}
	}
	return out, nil
}
