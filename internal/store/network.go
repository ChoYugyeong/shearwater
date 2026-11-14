package store

import "context"

// NetworkView is one SQLite read snapshot. Rules and drain acknowledgements
// must describe the SAME revision: observing a new drain after installing an
// older route could otherwise authorize termination while it still accepts
// connections. The transaction is closed before any slow kernel operations.
type NetworkView struct {
	Services, EndpointSlices, Pods []Record
	PodIPs                         map[string]string
}

func (s *Store) ServiceNetwork(ctx context.Context) (NetworkView, error) {
	var view NetworkView
	tx, err := s.r.BeginTx(ctx, nil)
	if err != nil {
		return view, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT grp,resource,namespace,name,uid,rev,generation,deleting,data FROM objects
		WHERE (grp='' AND resource IN ('services','pods')) OR (grp='discovery.k8s.io' AND resource='endpointslices') ORDER BY grp,resource,namespace,name`)
	if err != nil {
		return view, err
	}
	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.Key.Group, &rec.Key.Resource, &rec.Key.Namespace, &rec.Key.Name, &rec.UID, &rec.Rev, &rec.Generation, &rec.Deleting, &rec.Data); err != nil {
			rows.Close()
			return view, err
		}
		switch rec.Key.Resource {
		case "services":
			view.Services = append(view.Services, rec)
		case "pods":
			view.Pods = append(view.Pods, rec)
		case "endpointslices":
			view.EndpointSlices = append(view.EndpointSlices, rec)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return view, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT owner_uid,ip FROM ip_alloc WHERE kind='pod'`)
	if err != nil {
		return view, err
	}
	defer rows.Close()
	view.PodIPs = make(map[string]string)
	for rows.Next() {
		var uid, ip string
		if err := rows.Scan(&uid, &ip); err != nil {
			return view, err
		}
		view.PodIPs[uid] = ip
	}
	return view, rows.Err()
}
