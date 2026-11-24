// Package quota accounts for namespaced objects under the store's writer lock.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"math/big"
	"slices"
	"strings"
	"time"

	"shearwater/internal/admission"
	"shearwater/internal/apis"
	"shearwater/internal/objects"
	"shearwater/internal/store"
)

type Controller struct {
	store *store.Store
}

func New(s *store.Store) *Controller { return &Controller{store: s} }

type item struct {
	key     store.Key
	active  bool
	pod     *apis.Pod
	storage apis.Quantity
}

func load(key store.Key, deleting bool, data []byte) (item, error) {
	i := item{key: key, active: !deleting}
	if !i.active || key.Group != "" {
		return i, nil
	}
	switch key.Resource {
	case "pods":
		i.pod = new(apis.Pod)
		if err := json.Unmarshal(data, i.pod); err != nil {
			return i, err
		}
		i.active = i.pod.Status.Phase != "Succeeded" && i.pod.Status.Phase != "Failed"
	case "persistentvolumeclaims":
		var claim struct {
			Spec struct {
				Resources apis.ResourceRequirements
			}
		}
		if err := json.Unmarshal(data, &claim); err != nil {
			return i, err
		}
		i.storage = claim.Spec.Resources.Requests["storage"]
	}
	return i, nil
}

func (i item) amount(key string, require bool) (*big.Rat, error) {
	total := new(big.Rat)
	if !i.active {
		return total, nil
	}
	name := i.key.Resource
	if i.key.Group != "" {
		name += "." + i.key.Group
	}
	if key == "count/"+name || (key == name && apis.QuotaResourceSupported(key)) {
		return total.SetInt64(1), nil
	}
	if key == "requests.storage" && i.storage != "" {
		value, err := apis.ParseQuantity(string(i.storage))
		if err != nil {
			return nil, err
		}
		if value.Sign() < 0 {
			if require {
				return nil, fmt.Errorf("%s must be non-negative", key)
			}
			return total, nil
		}
		return value, nil
	}
	if i.pod == nil {
		return total, nil
	}
	scope, resource, ok := strings.Cut(key, ".")
	if !ok || (scope != "requests" && scope != "limits") || (resource != "cpu" && resource != "memory") {
		return total, nil
	}
	for _, c := range i.pod.Spec.Containers {
		value := c.Resources.Limits[resource]
		if scope == "requests" {
			// Kubernetes defaults an omitted request to the corresponding limit.
			if request, ok := c.Resources.Requests[resource]; ok {
				value = request
			}
		}
		if value == "" {
			if require {
				return nil, fmt.Errorf("must specify %s for: %s", key, c.Name)
			}
			continue
		}
		n, err := apis.ParseQuantity(string(value))
		if err != nil {
			return nil, err
		}
		if n.Sign() < 0 {
			if require {
				return nil, fmt.Errorf("%s must be non-negative for: %s", key, c.Name)
			}
			continue
		}
		total.Add(total, n)
	}
	return total, nil
}

func quantity(n *big.Rat) apis.Quantity {
	if n.IsInt() {
		return apis.Quantity(n.Num().String())
	}
	milli := new(big.Rat).Mul(n, big.NewRat(1000, 1))
	if milli.IsInt() {
		return apis.Quantity(milli.Num().String() + "m")
	}
	places, _ := n.FloatPrec()
	return apis.Quantity(n.FloatString(places))
}

func usage(items []item, key string) (*big.Rat, error) {
	total := new(big.Rat)
	for _, i := range items {
		n, err := i.amount(key, false)
		if err != nil {
			return nil, err
		}
		total.Add(total, n)
	}
	return total, nil
}

// itemsIn enumerates only tracked resources using the (group,resource,namespace)
// index. Read each matching object from the admission transaction itself.
func (c *Controller) itemsIn(tx *store.Tx, ns string, hard map[string]apis.Quantity) ([]item, error) {
	resources := make(map[string]bool)
	for key := range hard {
		if !apis.QuotaResourceSupported(key) {
			continue
		}
		switch key {
		case "requests.cpu", "requests.memory", "limits.cpu", "limits.memory":
			resources["pods"] = true
		case "requests.storage":
			resources["persistentvolumeclaims"] = true
		default:
			resources[strings.TrimPrefix(key, "count/")] = true
		}
	}
	var items []item
	for name := range resources {
		resource, group, _ := strings.Cut(name, ".")
		rows, _, err := c.store.List(group, resource, ns)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			current, err := tx.Get(row.Key)
			if err != nil {
				return nil, err
			}
			i, err := load(current.Key, current.Deleting, current.Data)
			if err != nil {
				return nil, err
			}
			items = append(items, i)
		}
	}
	return items, nil
}

func check(q *apis.ResourceQuota, items []item, next, prev item) error {
	const hint = "Reduce the request or ask an administrator to raise the namespace quota."
	for _, key := range slices.Sorted(maps.Keys(q.Spec.Hard)) {
		if !apis.QuotaResourceSupported(key) {
			continue
		}
		requested, err := next.amount(key, true)
		if err != nil {
			return admission.Forbid("QUOTA_REQUIRED", hint, "failed quota: %s: %s", q.Metadata.Name, err)
		}
		old, err := prev.amount(key, false)
		if err != nil {
			return err
		}
		delta := new(big.Rat).Sub(requested, old)
		if delta.Sign() <= 0 {
			continue
		}
		used, err := usage(items, key)
		if err != nil {
			return err
		}
		limit, err := apis.ParseQuantity(string(q.Spec.Hard[key]))
		if err != nil {
			return err
		}
		if new(big.Rat).Add(used, delta).Cmp(limit) > 0 {
			return admission.Forbid("QUOTA_EXCEEDED", hint, "exceeded quota: %s, requested: %s=%s, used: %s=%s, limited: %s=%s",
				q.Metadata.Name, key, quantity(delta), key, quantity(used), key, q.Spec.Hard[key])
		}
	}
	return nil
}

func (c *Controller) Admit(r admission.Request) error {
	if r.Namespace == "" || (r.Operation != admission.Create && r.Resource.Plural != "pods" && r.Resource.Plural != "persistentvolumeclaims") {
		return nil
	}
	// The writer lock keeps the quota index stable until this admission commits.
	rows, _, err := c.store.List("", "resourcequotas", r.Namespace)
	if err != nil || len(rows) == 0 {
		return err
	}
	hard := make(map[string]apis.Quantity)
	var quotas []apis.ResourceQuota
	for _, row := range rows {
		if row.Deleting {
			continue
		}
		var q apis.ResourceQuota
		if err := json.Unmarshal(row.Data, &q); err != nil {
			return err
		}
		quotas = append(quotas, q)
		maps.Copy(hard, q.Spec.Hard)
	}
	data, err := json.Marshal(r.Object)
	if err != nil {
		return err
	}
	key := objects.Key(r.Resource, r.Namespace, r.Name)
	next, err := load(key, r.Object.Meta().DeletionTimestamp != nil, data)
	if err != nil {
		return err
	}
	// Most writes do not consume any of the namespace's tracked resources.
	relevant := false
	for k := range hard {
		n, err := next.amount(k, true)
		if err != nil || n.Sign() > 0 {
			relevant = true
			break
		}
	}
	if !relevant {
		return nil
	}
	items, err := c.itemsIn(r.Tx, r.Namespace, hard)
	if err != nil {
		return err
	}
	var prev item
	for _, i := range items {
		if i.key == key {
			prev = i
			break
		}
	}
	for _, q := range quotas {
		if err := check(&q, items, next, prev); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) refresh(key store.Key) error {
	return c.store.Write(false, func(tx *store.Tx) error {
		row, err := tx.Get(key)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil || row.Deleting {
			return err
		}
		var q apis.ResourceQuota
		if err := json.Unmarshal(row.Data, &q); err != nil {
			return err
		}
		items, err := c.itemsIn(tx, key.Namespace, q.Spec.Hard)
		if err != nil {
			return err
		}
		used := make(map[string]apis.Quantity)
		for k := range q.Spec.Hard {
			if apis.QuotaResourceSupported(k) {
				v, err := usage(items, k)
				if err != nil {
					return err
				}
				used[k] = quantity(v)
			}
		}
		if maps.Equal(q.Status.Hard, q.Spec.Hard) && maps.Equal(q.Status.Used, used) {
			return nil
		}
		q.Status = apis.ResourceQuotaStatus{Hard: q.Spec.Hard, Used: used}
		_, _, err = tx.Update(objects.Put(apis.Lookup("", "v1", "resourcequotas"), &q), row.Rev)
		return err
	})
}

// Run coalesces workload changes into status updates. Admission always uses live usage.
func (c *Controller) Run(ctx context.Context) {
	for ctx.Err() == nil {
		changed := c.store.Changed()
		rows, _, err := c.store.List("", "resourcequotas", "")
		if err != nil {
			log.Printf("resource quota status: %v", err)
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			if err := c.refresh(row.Key); err != nil {
				log.Printf("resource quota status %s: %v", row.Key, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-time.After(time.Minute):
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
