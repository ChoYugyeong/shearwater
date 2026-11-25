package store

// Who changed what (design-agent-first-ha 2.3.2). A write made on behalf of a
// known caller records that caller next to every change-log entry it adds, in
// the same transaction: the API names the authenticated user (the
// administrator, a person or an agent), the node API names the worker node,
// delegated operations name the task. Controllers inside the server write
// without one and read back as unattributed.
//
// The table is keyed by the change revision and created on first start, not
// part of the schema version: an older sw opening the same database ignores
// it and simply leaves its own changes unattributed.

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// Actor is who made a change.
type Actor struct {
	Name string // admin, a user or agent name, system:node:NAME, task:ID
	Via  string // kubectl, api, node, task
}

// InitActors creates the attribution table. Writes before it (or on a store
// opened by a tool that never calls it) are not attributed.
func (s *Store) InitActors() error {
	if err := s.createTable(`CREATE TABLE IF NOT EXISTS change_actors (
		rev INTEGER PRIMARY KEY, actor TEXT NOT NULL, via TEXT NOT NULL)`, "change_actors"); err != nil {
		return err
	}
	s.mu.Lock()
	s.actors = true
	s.mu.Unlock()
	return nil
}

// createTable creates a table added after the first release. It writes (and
// replicates) only when the table is missing, so opening an up-to-date
// database stays free of writes.
func (s *Store) createTable(ddl, name string) error {
	if ok, err := hasTable(s.r, name); err != nil || ok {
		return err
	}
	return s.Write(false, func(tx *Tx) error {
		_, err := tx.execBookkeeping(ddl)
		return err
	})
}

func hasTable(q querier, name string) (bool, error) {
	var n int
	err := q.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0, err
}

// WriteAs is Write with a recorded as the maker of every change the
// transaction adds to the change log.
func (s *Store) WriteAs(a Actor, dryRun bool, fn func(tx *Tx) error) error {
	return s.write(a, dryRun, fn)
}

// Entry is one change-log entry with its commit time and, when known, who
// made it.
type Entry struct {
	Change
	At    string // RFC3339
	Actor Actor  // zero when unattributed
}

// History returns change-log entries of every resource after revision after,
// oldest first, at most limit. namespace "" means every namespace and the
// cluster-scoped objects; otherwise only that namespace. next and
// ErrCompacted work as in Changes. A negative after returns the newest limit
// entries instead, still oldest first.
func (s *Store) History(after int64, namespace string, limit int) (entries []Entry, next int64, err error) {
	if limit <= 0 {
		return nil, 0, fmt.Errorf("store: History needs a positive limit, got %d", limit)
	}
	tx, err := s.r.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	compacted, err := readMetaInt(tx, "compacted_rev")
	if err != nil {
		return nil, 0, err
	}
	if after >= 0 && after < compacted {
		return nil, 0, ErrCompacted
	}
	if next, err = readRev(tx); err != nil {
		return nil, 0, err
	}
	attributed, err := hasTable(tx, "change_actors")
	if err != nil {
		return nil, 0, err
	}
	who := `'', ''`
	join := ``
	if attributed {
		who = `COALESCE(a.actor, ''), COALESCE(a.via, '')`
		join = ` LEFT JOIN change_actors a ON a.rev = c.rev`
	}
	q := `SELECT c.rev, c.grp, c.resource, c.namespace, c.name, c.type, c.data, c.prev_data, c.at, ` + who +
		` FROM changes c` + join + ` WHERE `
	var args []any
	if after >= 0 {
		q += `c.rev > ?`
		args = append(args, after)
	} else {
		q += `1`
	}
	if namespace != "" {
		q += ` AND c.namespace = ?`
		args = append(args, namespace)
	}
	if after >= 0 {
		q += ` ORDER BY c.rev LIMIT ?`
	} else {
		q += ` ORDER BY c.rev DESC LIMIT ?`
	}
	rows, err := tx.Query(q, append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var prev []byte
		if err := rows.Scan(&e.Rev, &e.Key.Group, &e.Key.Resource, &e.Key.Namespace, &e.Key.Name, &e.Type,
			&e.Data, &prev, &e.At, &e.Actor.Name, &e.Actor.Via); err != nil {
			return nil, 0, err
		}
		e.Prev = prev
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if after < 0 {
		slices.Reverse(entries)
	} else if len(entries) == limit {
		next = entries[len(entries)-1].Rev
	}
	return entries, next, nil
}

// actorOf reads the attribution of one revision (tests and diagnostics).
func (s *Store) actorOf(rev int64) (Actor, error) {
	var a Actor
	err := s.r.QueryRow(`SELECT actor, via FROM change_actors WHERE rev = ?`, rev).Scan(&a.Name, &a.Via)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}
