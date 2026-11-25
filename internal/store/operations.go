package store

// Operation records are private server bookkeeping, not Kubernetes objects.
// Most importantly, the record of an applied plan and the Deployment update use
// the SAME Tx. A client losing the HTTP response can ask for the original plan
// again without guessing whether to repeat the write.
import "database/sql"

func (s *Store) InitOperations() error {
	return s.createTable(`CREATE TABLE IF NOT EXISTS agent_operations (id TEXT PRIMARY KEY, data BLOB NOT NULL) WITHOUT ROWID`, "agent_operations")
}

func (t *Tx) Operation(id string) ([]byte, error) {
	var data []byte
	err := t.tx.QueryRow(`SELECT data FROM agent_operations WHERE id = ?`, id).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return data, err
}

func (t *Tx) PutOperation(id string, data []byte, create bool) error {
	var err error
	if create {
		_, err = t.exec(`INSERT INTO agent_operations(id, data) VALUES (?, ?)`, id, data)
	} else {
		var result sql.Result
		result, err = t.exec(`UPDATE agent_operations SET data = ? WHERE id = ?`, data, id)
		if err == nil {
			n, e := result.RowsAffected()
			if e != nil {
				return e
			}
			if n != 1 {
				return ErrNotFound
			}
		}
	}
	if err == nil {
		t.bookkeepingChanged = true
	}
	return err
}

// ListObjects reads a namespace inside the caller's transaction. Unlike a
// sequence of REST requests, Deployment/ReplicaSet/Pod/Service observations then
// have one consistent database view. Nothing outside that namespace is loaded.
func (t *Tx) ListObjects(namespace string) ([]Record, error) {
	rows, err := t.tx.Query(`SELECT grp,resource,namespace,name,uid,rev,generation,deleting,data FROM objects WHERE namespace = ? ORDER BY grp,resource,name`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.Key.Group, &r.Key.Resource, &r.Key.Namespace, &r.Key.Name, &r.UID, &r.Rev, &r.Generation, &r.Deleting, &r.Data); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
