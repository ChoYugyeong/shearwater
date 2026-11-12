package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

// serviceIPCandidates walks the IPv4 range in address order, excluding the
// network and broadcast addresses. There is no Service gateway to reserve.
// M4 accepted the top address as an explicit IP, so explicit validation
// continues to accept it. The proxy binds individual addresses as /32s.
func serviceIPCandidates(yield func(string) bool) {
	cidr := apis.ServiceCIDR.Masked()
	for ip := cidr.Addr().Next(); cidr.Contains(ip) && cidr.Contains(ip.Next()); ip = ip.Next() {
		if !yield(ip.String()) {
			return
		}
	}
}

func allocateNewServiceIP(tx *store.Tx, res *apis.Resource, svc *apis.Service) error {
	m := svc.Meta()
	// Preserve AlreadyExists for a retry at the same name, even if its
	// explicit IP is now occupied by the original request. Both checks
	// are inside the same write transaction as the create.
	if _, err := tx.Get(keyOf(res, m.Namespace, m.Name)); err == nil {
		return store.ErrExists
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	ip, err := tx.AllocIP("service", m.UID, svc.Spec.ClusterIP, serviceIPCandidates)
	switch {
	case err == nil:
		svc.Spec.ClusterIP = ip
		return nil
	case errors.Is(err, store.ErrIPAllocated):
		return errInvalid(res, m.Name, apis.ErrorList{{
			Reason: "FieldValueInvalid",
			Field:  "spec.clusterIP",
			Detail: fmt.Sprintf("Invalid value: %q: the IP address is already allocated", svc.Spec.ClusterIP),
		}})
	case errors.Is(err, store.ErrIPExhausted):
		return newErr(http.StatusServiceUnavailable, "ServiceUnavailable", "IP_EXHAUSTED", "safe",
			"no free ClusterIP addresses in %s", apis.ServiceCIDR)
	default:
		return err
	}
}

// ReconcileServiceIPs upgrades M4 Service objects before HTTP and controllers
// start, including when the server runs in API-only mode. It is safe to call
// again at every startup. Duplicate explicit IPs abort the whole upgrade with
// both Service keys, preserving the IPs that users may already refer to.
func ReconcileServiceIPs(st *store.Store) error {
	res := apis.Lookup("", "v1", "services")
	type serviceRecord struct {
		record store.Record
		object apis.Service
	}
	return st.Write(false, func(tx *store.Tx) error {
		records, err := tx.Objects()
		if err != nil {
			return err
		}
		var explicit, unassigned []serviceRecord
		seen := make(map[string]store.Key)
		for _, rec := range records {
			if rec.Key.Group != res.Group || rec.Key.Resource != res.Plural {
				continue
			}
			var svc apis.Service
			if err := json.Unmarshal(rec.Data, &svc); err != nil {
				return fmt.Errorf("read stored %s: %w", rec.Key, err)
			}
			switch ip := svc.Spec.ClusterIP; ip {
			case "None":
				// Headless Services keep their stored-only behavior.
			case "":
				unassigned = append(unassigned, serviceRecord{rec, svc})
			default:
				if errs := apis.ValidateServiceClusterIP(ip); len(errs) > 0 {
					return fmt.Errorf("stored %s has an invalid ClusterIP: %s", rec.Key, errs[0])
				}
				if other, ok := seen[ip]; ok {
					return fmt.Errorf("duplicate ClusterIP %q in %s and %s", ip, other, rec.Key)
				}
				seen[ip] = rec.Key
				explicit = append(explicit, serviceRecord{rec, svc})
			}
		}
		// Reserve every existing explicit address before allocating the first
		// blank one, even if the explicit Service sorts later or is terminating.
		// An explicit reservation by itself changes no object or revision.
		for _, svc := range explicit {
			ip := svc.object.Spec.ClusterIP
			if _, err := tx.AllocIP("service", svc.record.UID, ip, nil); err != nil {
				return fmt.Errorf("reserve ClusterIP %q for %s: %w", ip, svc.record.Key, err)
			}
		}
		for _, svc := range unassigned {
			rec := svc.record
			ip, err := tx.AllocIP("service", rec.UID, "", serviceIPCandidates)
			if err != nil {
				return fmt.Errorf("allocate ClusterIP for %s: %w", rec.Key, err)
			}
			// The spec of a blank Service changes. Let the normal store
			// update publish a revision and a MODIFIED event with the IP.
			svc.object.Spec.ClusterIP = ip
			svc.object.Metadata.Generation++
			if _, _, err := tx.Update(put(res, &svc.object), rec.Rev); err != nil {
				return fmt.Errorf("update %s with ClusterIP %q: %w", rec.Key, ip, err)
			}
		}
		return nil
	})
}
