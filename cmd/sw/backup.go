package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"shearwater/internal/api"
	"shearwater/internal/pki"
	"shearwater/internal/runtime"
)

type backupEntry struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256,omitempty"`
	Mode      uint32 `json:"mode"`
	UID       uint32 `json:"uid"`
	GID       uint32 `json:"gid"`
	Directory bool   `json:"directory,omitempty"`
}
type backupManifest struct {
	Format   int               `json:"format"`
	Version  string            `json:"version"`
	Created  time.Time         `json:"created"`
	Source   opsOptions        `json:"source"`
	Cold     bool              `json:"cold"`
	Volumes  map[string]string `json:"volumes"` // original hostPath -> relative saved directory
	Excluded []string          `json:"excluded"`
	Entries  []backupEntry     `json:"entries"`
}

func sqliteRead(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	return sql.Open("sqlite", u.String()+"?mode=ro&_pragma=busy_timeout(5000)")
}
func verifyDB(path string) error {
	db, err := sqliteRead(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var ok, version string
	if err = db.QueryRow("PRAGMA integrity_check").Scan(&ok); err != nil {
		return err
	}
	if ok != "ok" {
		return fmt.Errorf("SQLite integrity: %s", ok)
	}
	if err = db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version); err != nil {
		return err
	}
	if version != "1" {
		return fmt.Errorf("unsupported database schema %q", version)
	}
	return nil
}

// A backup directory is private and new. No archive can choose an absolute
// restore destination, and no restore can overwrite an existing installation.
func newBackupDir(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
		return fmt.Errorf("destination must be an absolute new directory")
	}
	if err := checkPrivateDir(filepath.Dir(path), false); err != nil {
		return err
	}
	return os.Mkdir(path, 0700)
}
func backupTree(source, target, rel string, entries *[]backupEntry, budget *int64) error {
	// WalkDir rejects symlink children, but it would still follow a symlink in
	// the starting path's parents. Do not let an allowed hostPath spelling lead
	// the backup outside the declared tree (for example app/link -> /etc).
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	if resolved != filepath.Clean(source) {
		return fmt.Errorf("backup source contains a symlink: %s", source)
	}
	return filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if len(*entries) >= 100000 {
			return fmt.Errorf("backup exceeds 100000 entries")
		}
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported volume entry %s (symlinks and special files are not backed up)", path)
		}
		sub, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		name := filepath.Join(rel, sub)
		dst := filepath.Join(target, name)
		st := info.Sys().(*syscall.Stat_t)
		e := backupEntry{Path: filepath.ToSlash(name), Mode: uint32(info.Mode().Perm()), UID: st.Uid, GID: st.Gid, Directory: info.IsDir()}
		if info.IsDir() {
			if err = os.MkdirAll(dst, 0700); err != nil {
				return err
			}
		} else {
			if info.Size() > *budget {
				return fmt.Errorf("backup exceeds --max-bytes")
			}
			if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			defer f.Close()
			actual, err := f.Stat()
			if err != nil {
				return err
			}
			if !os.SameFile(info, actual) {
				return fmt.Errorf("source changed while backing up %s", path)
			}
			out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				return err
			}
			h := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(out, h), io.LimitReader(f, *budget+1))
			syncErr := out.Sync()
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if syncErr != nil {
				return syncErr
			}
			if closeErr != nil {
				return closeErr
			}
			if n != info.Size() || n > *budget {
				return fmt.Errorf("source changed or backup exceeds limit: %s", path)
			}
			after, err := f.Stat()
			if err != nil {
				return err
			}
			if !after.ModTime().Equal(info.ModTime()) {
				return fmt.Errorf("source changed during backup: %s", path)
			}
			*budget -= n
			e.Size = n
			e.SHA256 = hex.EncodeToString(h.Sum(nil))
		}
		*entries = append(*entries, e)
		return nil
	})
}

func hostPaths(dbpath string) ([]string, error) {
	db, err := sqliteRead(dbpath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT data FROM objects WHERE resource IN ('pods','deployments','replicasets')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := map[string]bool{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		var obj map[string]any
		if err = json.Unmarshal(b, &obj); err != nil {
			return nil, err
		}
		walkHostPaths(obj, func(path string) string { paths[path] = true; return path })
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for p := range paths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// Visit only Pod specs and workload templates, never arbitrary annotations.
func walkHostPaths(obj map[string]any, replace func(string) string) {
	spec, _ := obj["spec"].(map[string]any)
	if template, ok := spec["template"].(map[string]any); ok {
		spec, _ = template["spec"].(map[string]any)
	}
	volumes, _ := spec["volumes"].([]any)
	for _, v := range volumes {
		vol, _ := v.(map[string]any)
		hp, _ := vol["hostPath"].(map[string]any)
		if p, ok := hp["path"].(string); ok {
			hp["path"] = replace(p)
		}
	}
}

func runBackup(args []string, out io.Writer) error {
	f := flag.NewFlagSet("backup", flag.ContinueOnError)
	var o opsOptions
	o.flags(f)
	dest := f.String("output", "", "new private backup directory")
	cold := f.Bool("include-volumes", false, "include emptyDir and declared regular-file/directory hostPath data; server and tasks must be stopped")
	limit := f.Int64("max-bytes", 1<<30, "maximum copied bytes (default 1 GiB)")
	if err := parseOps(f, args, &o); err != nil {
		return err
	}
	if *limit < 1 || *limit > 1<<40 {
		return fmt.Errorf("max-bytes must be between 1 and 1 TiB")
	}
	if err := privateDir(o.DataDir, false); err != nil {
		return err
	}
	if err := privateDir(o.ConfigDir, false); err != nil {
		return err
	}
	if _, err := pki.Load(o.ConfigDir); err != nil {
		return err
	}
	if *cold {
		// The same lock is held by every new foreground/managed server. A separate
		// runtime check also covers apps whose shims survived a server stop.
		lock, err := fileLock(o.DataDir, "database.lock")
		if err != nil {
			return fmt.Errorf("stop the server before a volume backup: %w", err)
		}
		defer lock.Close()
		if st, err := readState(o.RunDir); err == nil {
			fd, e := st.openProcess()
			if e != nil {
				return e
			}
			if fd >= 0 {
				syscall.Close(fd)
				return fmt.Errorf("server is still running; stop it and its apps before including volumes")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			// Older servers do not hold database.lock. An unreadable process
			// receipt must not be mistaken for proof that such a server stopped.
			return fmt.Errorf("cannot verify that the server stopped: %w", err)
		}
		if o.Runtime == "containerd" {
			lock, err := lockNode()
			if err != nil {
				return err
			}
			defer lock.Close()
			r := runtime.NewContainerd(o.Socket)
			defer r.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cs, err := r.Containers(ctx, "")
			if err != nil {
				return err
			}
			if len(cs) != 0 {
				return fmt.Errorf("runtime still contains %d containers; cold backup requires no tasks or containers", len(cs))
			}
		}
	}
	if err := newBackupDir(*dest); err != nil {
		return err
	}
	// Incomplete directories are retained with no manifest, so failure never
	// destroys a previous backup or leaves something that verifies as complete.
	if err := os.Mkdir(filepath.Join(*dest, "data"), 0700); err != nil {
		return err
	}
	db, err := sqliteRead(filepath.Join(o.DataDir, "state.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	snapshot := filepath.Join(*dest, "data", "state.db")
	if _, err = db.Exec("VACUUM INTO ?", snapshot); err != nil {
		return err
	}
	if err = os.Chmod(snapshot, 0600); err != nil {
		return err
	}
	if err = verifyDB(snapshot); err != nil {
		return err
	}
	m := backupManifest{Format: 1, Version: api.Version, Created: time.Now().UTC(), Source: o, Cold: *cold, Volumes: map[string]string{}, Excluded: []string{"container images and writable image layers", "logs", "runtime sockets and network namespaces", "browser localStorage and external databases"}}
	fdb, err := os.Open(snapshot)
	if err != nil {
		return err
	}
	h := sha256.New()
	size, err := io.Copy(h, fdb)
	fdb.Close()
	if err != nil {
		return err
	}
	m.Entries = append(m.Entries, backupEntry{Path: "data/state.db", Size: size, SHA256: hex.EncodeToString(h.Sum(nil)), Mode: 0600, UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())})
	*limit -= size
	if *limit < 0 {
		return fmt.Errorf("database exceeds max-bytes")
	}
	for _, name := range []string{"ca.crt", "ca.key", "server.crt", "server.key", "admin.token"} {
		if err = backupTree(filepath.Join(o.ConfigDir, name), *dest, "config/"+name, &m.Entries, limit); err != nil {
			return err
		}
	}
	if *cold {
		if _, err = os.Stat(filepath.Join(o.DataDir, "pods")); err == nil {
			if err = backupTree(filepath.Join(o.DataDir, "pods"), *dest, "data/pods", &m.Entries, limit); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		paths, err := hostPaths(snapshot)
		if err != nil {
			return err
		}
		for i, path := range paths {
			// hostPath can refer to the whole machine. A backup never recursively
			// collects a system root just because a manifest happened to mount it.
			clean := filepath.Clean(path)
			allowed := strings.HasPrefix(clean, "/opt/shearwater/") || strings.HasPrefix(clean, "/var/lib/shearwater/")
			if !allowed || clean == o.DataDir || strings.HasPrefix(filepath.Clean(*dest)+"/", clean+"/") {
				return fmt.Errorf("hostPath %s is outside supported backup roots or contains this backup; use an application-specific backup", path)
			}
			rel := fmt.Sprintf("volumes/%04d", i)
			if err = backupTree(clean, *dest, rel, &m.Entries, limit); err != nil {
				return err
			}
			m.Volumes[path] = rel
		}
	} else {
		m.Excluded = append(m.Excluded, "all emptyDir and hostPath contents (control-plane-only backup)")
	}
	if err = atomicJSON(filepath.Join(*dest, "manifest.json"), m); err != nil {
		return err
	}
	fmt.Fprintf(out, "Backup complete: %s\nmode=%s files=%d\nExcluded: %s\n", *dest, map[bool]string{true: "cold-volumes", false: "control-plane-only"}[*cold], len(m.Entries), strings.Join(m.Excluded, "; "))
	return nil
}

func loadBackup(dir string) (backupManifest, error) {
	var m backupManifest
	if !filepath.IsAbs(dir) {
		return m, fmt.Errorf("backup path must be absolute")
	}
	if err := privateDir(dir, false); err != nil {
		return m, err
	}
	manifestFile, err := openPrivate(filepath.Join(dir, "manifest.json"), os.O_RDONLY)
	if err != nil {
		return m, err
	}
	defer manifestFile.Close()
	b, err := io.ReadAll(io.LimitReader(manifestFile, (8<<20)+1))
	if err != nil {
		return m, err
	}
	if len(b) > 8<<20 {
		return m, fmt.Errorf("backup manifest too large")
	}
	if err = json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if m.Format != 1 {
		return m, fmt.Errorf("unsupported backup format")
	}
	seen := map[string]bool{}
	for _, e := range m.Entries {
		if !filepath.IsLocal(e.Path) || filepath.ToSlash(filepath.Clean(e.Path)) != e.Path || seen[e.Path] || (!strings.HasPrefix(e.Path, "data/") && !strings.HasPrefix(e.Path, "config/") && !strings.HasPrefix(e.Path, "volumes/")) {
			return m, fmt.Errorf("invalid/duplicate backup path %q", e.Path)
		}
		seen[e.Path] = true
		for parent := filepath.Dir(e.Path); parent != "."; parent = filepath.Dir(parent) {
			info, err := os.Lstat(filepath.Join(dir, parent))
			if err != nil {
				return m, err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return m, fmt.Errorf("unsafe backup parent: %s", parent)
			}
		}
		info, err := os.Lstat(filepath.Join(dir, e.Path))
		if err != nil {
			return m, err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != e.Directory {
			return m, fmt.Errorf("backup entry type mismatch: %s", e.Path)
		}
		if e.Directory {
			continue
		}
		if !info.Mode().IsRegular() || info.Size() != e.Size {
			return m, fmt.Errorf("backup entry size/type mismatch: %s", e.Path)
		}
		f, err := os.OpenInRoot(dir, e.Path)
		if err != nil {
			return m, err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return m, err
		}
		if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
			return m, fmt.Errorf("checksum mismatch: %s", e.Path)
		}
	}
	for _, p := range []string{"data/state.db", "config/ca.crt", "config/ca.key", "config/server.crt", "config/server.key", "config/admin.token"} {
		if !seen[p] {
			return m, fmt.Errorf("required backup entry missing: %s", p)
		}
	}
	for _, p := range m.Volumes {
		if !seen[p] || !strings.HasPrefix(p, "volumes/") {
			return m, fmt.Errorf("invalid volume mapping")
		}
	}
	if err = verifyDB(filepath.Join(dir, "data/state.db")); err != nil {
		return m, err
	}
	_, err = pki.Load(filepath.Join(dir, "config"))
	return m, err
}

func runRestore(args []string, out io.Writer) error {
	f := flag.NewFlagSet("restore", flag.ContinueOnError)
	from := f.String("from", "", "backup directory")
	to := f.String("to", "", "new restore directory; never overwritten")
	verify := f.Bool("verify-only", false, "verify checksums and SQLite without restoring")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected restore arguments")
	}
	m, err := loadBackup(*from)
	if err != nil {
		return err
	}
	if *verify {
		fmt.Fprintf(out, "Backup verified: %d entries, cold=%t\n", len(m.Entries), m.Cold)
		return nil
	}
	if err = newBackupDir(*to); err != nil {
		return err
	}
	// Copy only manifest entries. Extraneous files added beside a backup do not
	// become executable configuration, and imported owner locks are never used.
	for _, e := range m.Entries {
		dst := filepath.Join(*to, e.Path)
		if e.Directory {
			if err = os.MkdirAll(dst, 0700); err != nil {
				return err
			}
			continue
		}
		if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		src, err := os.OpenInRoot(*from, e.Path)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			src.Close()
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(src, e.Size+1))
		src.Close()
		syncErr := f.Sync()
		f.Close()
		if err != nil {
			return err
		}
		if syncErr != nil {
			return syncErr
		}
		if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
			return fmt.Errorf("backup changed while restoring %s; incomplete target retained", e.Path)
		}
	}
	// Permissions are applied last; copying into a read-only directory would
	// otherwise fail halfway. Volume uid/gid are meaningful inside containers.
	for i := len(m.Entries) - 1; i >= 0; i-- {
		e := m.Entries[i]
		dst := filepath.Join(*to, e.Path)
		if os.Geteuid() == 0 {
			if err = os.Chown(dst, int(e.UID), int(e.GID)); err != nil {
				return err
			}
		}
		mode := os.FileMode(e.Mode) & 0777
		if strings.HasPrefix(e.Path, "config/") {
			mode = 0600
		}
		if err = os.Chmod(dst, mode); err != nil {
			return err
		}
	}
	// Keep the original hostPath targets explicit. Automatic relocation would
	// edit declarative resources and invalidate their revision history. The
	// restored server is never started here; the operator reviews this mapping.
	fmt.Fprintf(out, "Restored and verified into %s. Nothing was started.\n--data-dir=%s --config-dir=%s\n", *to, filepath.Join(*to, "data"), filepath.Join(*to, "config"))
	if !m.Cold {
		fmt.Fprintln(out, "This backup excludes app volumes. Restore app data separately before starting workloads.")
	}
	if len(m.Volumes) > 0 {
		fmt.Fprintln(out, "HostPath files were extracted separately; manifests still use their original paths. Review volumes before startup:")
		for original, rel := range m.Volumes {
			fmt.Fprintf(out, "  %s <- %s\n", original, filepath.Join(*to, rel))
		}
	}
	return atomicJSON(filepath.Join(*to, "RESTORED.json"), m)
}
