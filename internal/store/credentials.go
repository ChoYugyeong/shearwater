package store

// Credentials are hashed bearer tokens for users, worker nodes and join
// tokens. Like agent operations they are private server bookkeeping: not
// Kubernetes objects, never in the change log, never visible through a watch.
// Only the hash of a token's secret part is stored.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type Credential struct {
	ID      string // the public part of the token
	Kind    string // user, node or join (auth.Kind*)
	Name    string // user name, node name, or a label for a join token
	Groups  []string
	Hash    string // hex SHA-256 of the secret part
	Created time.Time
	Expires time.Time       // zero: does not expire
	Data    json.RawMessage // kind-specific details; may be nil
}

func (s *Store) InitCredentials() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.w.Exec(`CREATE TABLE IF NOT EXISTS credentials (
		id TEXT PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL, groups TEXT NOT NULL,
		hash TEXT NOT NULL, created TEXT NOT NULL, expires TEXT NOT NULL, data BLOB) WITHOUT ROWID`)
	return err
}

const credentialColumns = `id, kind, name, groups, hash, created, expires, data`

type scanner interface{ Scan(dest ...any) error }

func scanCredential(row scanner) (Credential, error) {
	var c Credential
	var groups, created, expires string
	var data []byte // json.RawMessage cannot take a NULL from database/sql
	if err := row.Scan(&c.ID, &c.Kind, &c.Name, &groups, &c.Hash, &created, &expires, &data); err != nil {
		return c, err
	}
	if len(data) > 0 {
		c.Data = data
	}
	if err := json.Unmarshal([]byte(groups), &c.Groups); err != nil {
		return c, err
	}
	var err error
	if c.Created, err = time.Parse(time.RFC3339, created); err != nil {
		return c, err
	}
	if expires != "" {
		if c.Expires, err = time.Parse(time.RFC3339, expires); err != nil {
			return c, err
		}
	}
	return c, nil
}

// Credential returns one credential by its public id.
func (s *Store) Credential(id string) (Credential, error) {
	c, err := scanCredential(s.r.QueryRow(`SELECT `+credentialColumns+` FROM credentials WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil && isMissingTable(err) {
		return c, ErrNotFound
	}
	return c, err
}

// Credentials lists the credentials of one kind ("" for all), oldest first.
func (s *Store) Credentials(kind string) ([]Credential, error) {
	q, args := `SELECT `+credentialColumns+` FROM credentials`, []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	rows, err := s.r.Query(q+` ORDER BY created, id`, args...)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutCredential stores c, replacing a credential with the same id.
func (s *Store) PutCredential(c Credential) error {
	groups, err := json.Marshal(c.Groups)
	if err != nil {
		return err
	}
	if c.Groups == nil {
		groups = []byte("[]")
	}
	expires := ""
	if !c.Expires.IsZero() {
		expires = c.Expires.UTC().Format(time.RFC3339)
	}
	if c.Created.IsZero() {
		c.Created = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Exec(`INSERT INTO credentials (`+credentialColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET kind = excluded.kind, name = excluded.name, groups = excluded.groups,
		hash = excluded.hash, created = excluded.created, expires = excluded.expires, data = excluded.data`,
		c.ID, c.Kind, c.Name, string(groups), c.Hash, c.Created.UTC().Format(time.RFC3339), expires, []byte(c.Data))
	return err
}

// DeleteCredential removes one credential. A missing one is not an error.
func (s *Store) DeleteCredential(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.w.Exec(`DELETE FROM credentials WHERE id = ?`, id)
	return err
}

// DeleteCredentials removes every credential of one kind and name and says
// how many there were.
func (s *Store) DeleteCredentials(kind, name string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.w.Exec(`DELETE FROM credentials WHERE kind = ? AND name = ?`, kind, name)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func isMissingTable(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such table")
}
