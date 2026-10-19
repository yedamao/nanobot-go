package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/providers"

	// Pure-Go SQLite driver, registered as "sqlite"; no cgo needed.
	_ "modernc.org/sqlite"
)

const (
	// SQLiteFileName is the database file inside a workspace's sessions
	// directory.
	SQLiteFileName = "sessions.sqlite3"
	// sqliteBusyTimeout bounds how long a connection waits for another
	// writer (another process on the same workspace) before failing.
	sqliteBusyTimeout = 5 * time.Second

	// metaJSONLImport records that the one-time JSONL import ran. Its only
	// valid value is "complete".
	metaJSONLImport = "jsonl_import"
	// metaJSONLRename lists, as a JSON array of paths, the imported JSONL
	// files still to be renamed. It commits with the import, so a crash
	// before the renames finish is completed on the next open.
	metaJSONLRename = "jsonl_import_rename"
	// MigratedSuffix is appended to JSONL session files once they are
	// imported into SQLite. The JSONL store ignores the renamed files.
	MigratedSuffix = ".migrated"
)

// SQLiteStore keeps every session of one workspace in a SQLite database,
// porting nanobot's SqliteSessionStore: one row per session (metadata,
// consolidation marker and summary, generation) and one row per message.
//
// Each successful Save advances the session's generation; a Save whose
// Session.Generation does not match the stored one fails with
// ErrSessionConflict, so two processes sharing a workspace cannot silently
// overwrite each other's turns. The database runs in WAL mode with a busy
// timeout, and every operation is one transaction (BEGIN IMMEDIATE for
// writes), so a crash leaves the previous or the new state.
type SQLiteStore struct {
	db   *sql.DB
	path string
}

var _ Store = (*SQLiteStore)(nil)

// OpenSQLite opens the database at path, creating it and its directory
// (mode 0700, file 0600) if needed, and upgrades its schema. The first
// time a database is opened, the JSONL sessions in importDir (if not
// empty) are imported in the same transaction, then renamed to
// <name>.jsonl.migrated. A JSONL file that cannot be read fails the open
// and nothing is imported, so the sources stay as they were.
func OpenSQLite(path, importDir string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("open session database: %w", err)
	}
	if err := createPrivateFile(path); err != nil {
		return nil, fmt.Errorf("open session database: %w", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open session database %s: %w", path, err)
	}
	s := &SQLiteStore{db: db, path: path}
	if err := s.init(importDir); err != nil {
		db.Close()
		return nil, fmt.Errorf("open session database %s: %w", path, err)
	}
	return s, nil
}

// createPrivateFile creates path with mode 0600 unless it exists, so the
// database (and the WAL files SQLite creates with the same mode) is never
// readable by others. A symlink or other non-regular file is refused.
func createPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return createPrivateFile(path) // created concurrently; check it
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// sqliteDSN builds a URI filename (SQLite decodes the percent-escapes)
// with the per-connection settings: busy timeout, foreign keys for the
// ON DELETE CASCADE, full fsync on commit, and BEGIN IMMEDIATE for write
// transactions so two writers queue on the busy timeout instead of
// deadlocking when one upgrades a read lock.
func sqliteDSN(path string) string {
	q := url.Values{}
	q.Set("_busy_timeout", fmt.Sprint(sqliteBusyTimeout.Milliseconds()))
	q.Set("_foreign_keys", "1")
	q.Set("_synchronous", "FULL")
	q.Set("_txlock", "immediate")
	return "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath() + "?" + q.Encode()
}

// sqliteMigrations returns the schema upgrades: entry i upgrades version
// i to i+1. Released entries must never change; append new ones.
func sqliteMigrations() []func(*sql.Tx) error {
	return []func(*sql.Tx) error{migrateSQLiteV1}
}

func migrateSQLiteV1(tx *sql.Tx) error {
	return execAll(tx,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE sessions (
			key               TEXT PRIMARY KEY,
			created_at        INTEGER NOT NULL,
			updated_at        INTEGER NOT NULL,
			metadata          TEXT NOT NULL,
			last_consolidated INTEGER NOT NULL,
			summary           TEXT NOT NULL,
			generation        INTEGER NOT NULL
		)`,
		`CREATE TABLE messages (
			session_key TEXT NOT NULL REFERENCES sessions(key) ON DELETE CASCADE,
			position    INTEGER NOT NULL,
			record      TEXT NOT NULL,
			PRIMARY KEY (session_key, position)
		) WITHOUT ROWID`,
		`CREATE INDEX sessions_updated ON sessions(updated_at DESC)`,
	)
}

func execAll(tx *sql.Tx, stmts ...string) error {
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// init upgrades the schema and runs the JSONL import in one transaction,
// then switches to WAL and finishes renaming imported files.
func (s *SQLiteStore) init(importDir string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Recheck under the write lock in case another process upgraded first.
	version, err := schemaVersion(tx)
	if err != nil {
		return err
	}
	migrations := sqliteMigrations()
	if version > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this nanobot-go supports (%d); upgrade nanobot-go", version, len(migrations))
	}
	for v := version; v < len(migrations); v++ {
		if err := migrations[v](tx); err != nil {
			return fmt.Errorf("upgrade schema to version %d: %w", v+1, err)
		}
		if _, err := tx.Exec(`UPDATE schema_version SET version = ?`, v+1); err != nil {
			return err
		}
	}
	if err := importJSONL(tx, importDir); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// WAL is a persistent property of the file, set outside a transaction
	// once the schema is known to be ours.
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode); err != nil {
		return fmt.Errorf("enable WAL: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("enable WAL: journal mode is %q", mode)
	}
	return s.finishJSONLImport()
}

// schemaVersion reads the version from the single-row schema_version
// table, creating it at version 0 for a new database.
func schemaVersion(tx *sql.Tx) (int, error) {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(`SELECT version FROM schema_version`)
	if err != nil {
		return 0, err
	}
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return 0, err
		}
		versions = append(versions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	switch {
	case len(versions) == 0:
		_, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (0)`)
		return 0, err
	case len(versions) > 1 || versions[0] < 0:
		return 0, fmt.Errorf("corrupt schema_version table: %v", versions)
	}
	return versions[0], nil
}

// importJSONL imports the sessions in dir the first time the database is
// opened and records that it did, together with the files to rename.
func importJSONL(tx *sql.Tx, dir string) error {
	var done string
	err := tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaJSONLImport).Scan(&done)
	if err == nil {
		if done != "complete" {
			return fmt.Errorf("invalid JSONL import marker %q in table meta", done)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var imported []string
	if dir != "" {
		if dir, err = filepath.Abs(dir); err != nil {
			return fmt.Errorf("import JSONL sessions: %w", err)
		}
		names, err := NewJSONLStore(dir).files()
		if err != nil {
			return fmt.Errorf("import JSONL sessions: %w", err)
		}
		from := map[string]string{}
		for _, name := range names {
			path := filepath.Join(dir, name)
			sess, err := loadJSONLFile(path)
			if err != nil {
				return fmt.Errorf("import JSONL session %s: %w; nothing was imported: fix or move the file", path, err)
			}
			if prev, ok := from[sess.Key]; ok {
				return fmt.Errorf("import JSONL sessions: %s and %s both hold session %q; nothing was imported", prev, path, sess.Key)
			}
			from[sess.Key] = path
			sess.Generation = 0
			if _, err := saveTx(tx, sess); err != nil {
				return fmt.Errorf("import JSONL session %s: %w", path, err)
			}
			imported = append(imported, path)
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, 'complete')`, metaJSONLImport); err != nil {
		return err
	}
	if len(imported) == 0 {
		return nil
	}
	list, err := json.Marshal(imported)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`, metaJSONLRename, string(list))
	return err
}

// finishJSONLImport renames the imported JSONL files to *.migrated, so
// they stay on disk as a backup but are no longer read, then clears the
// pending list. Files already renamed (by a concurrent open) are skipped.
func (s *SQLiteStore) finishJSONLImport() error {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaJSONLRename).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var paths []string
	if err := json.Unmarshal([]byte(raw), &paths); err != nil {
		return fmt.Errorf("invalid %s in table meta: %w", metaJSONLRename, err)
	}
	for _, p := range paths {
		if !filepath.IsAbs(p) || !strings.HasSuffix(p, fileExt) {
			return fmt.Errorf("invalid %s entry %q in table meta", metaJSONLRename, p)
		}
		if err := os.Rename(p, p+MigratedSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("mark imported JSONL session %s: %w", p, err)
		}
	}
	_, err = s.db.Exec(`DELETE FROM meta WHERE key = ?`, metaJSONLRename)
	return err
}

// Path returns the database file.
func (s *SQLiteStore) Path() string { return s.path }

// Close closes the database.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// Load reads the session stored under key from one snapshot. It returns
// ErrNotFound when there is none. Message rows that do not decode to a
// valid message are skipped, as in the JSONL store.
func (s *SQLiteStore) Load(key string) (*Session, error) {
	sess, err := s.load(key)
	if err != nil {
		return nil, fmt.Errorf("load session %q: %w", key, err)
	}
	return sess, nil
}

func (s *SQLiteStore) load(key string) (*Session, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		created, updated int64
		meta, summary    string
		marker           int
	)
	sess := &Session{Key: key}
	err = tx.QueryRow(`SELECT created_at, updated_at, metadata, last_consolidated, summary, generation
		FROM sessions WHERE key = ?`, key).Scan(&created, &updated, &meta, &marker, &summary, &sess.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sess.CreatedAt, sess.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	if err := json.Unmarshal([]byte(meta), &sess.Metadata); err != nil {
		return nil, fmt.Errorf("bad metadata: %w", err)
	}
	if sess.Metadata == nil {
		sess.Metadata = map[string]any{}
	}

	rows, err := tx.Query(`SELECT record FROM messages WHERE session_key = ? ORDER BY position`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var record string
		if err := rows.Scan(&record); err != nil {
			return nil, err
		}
		var m providers.Message
		if json.Unmarshal([]byte(record), &m) == nil && validRole(m.Role) {
			sess.Messages = append(sess.Messages, m)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sess.setConsolidation(marker, summary)
	return sess, nil
}

// Save stores sess in one transaction and advances sess.Generation. It
// fails with ErrSessionConflict, changing nothing, when the stored
// generation differs from sess.Generation (another writer saved it) or
// when sess was stored before and has since been deleted.
func (s *SQLiteStore) Save(sess *Session) error {
	gen, err := s.save(sess)
	if err != nil {
		return fmt.Errorf("save session %q: %w", sess.Key, err)
	}
	sess.Generation = gen
	return nil
}

func (s *SQLiteStore) save(sess *Session) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	gen, err := saveTx(tx, sess)
	if err != nil {
		return 0, err
	}
	return gen, tx.Commit()
}

// saveTx writes sess inside tx and returns its new generation. Unchanged
// message rows are left alone, so an append writes only the new rows.
func saveTx(tx *sql.Tx, sess *Session) (int64, error) {
	if sess.LastConsolidated < 0 || sess.LastConsolidated > len(sess.Messages) {
		return 0, fmt.Errorf("consolidation marker %d outside [0, %d]", sess.LastConsolidated, len(sess.Messages))
	}
	metadata := sess.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	meta, err := encodeJSON(metadata)
	if err != nil {
		return 0, err
	}
	records := make([]string, len(sess.Messages))
	for i, m := range sess.Messages {
		if records[i], err = encodeJSON(m); err != nil {
			return 0, err
		}
	}

	var stored, next int64
	err = tx.QueryRow(`SELECT generation FROM sessions WHERE key = ?`, sess.Key).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if sess.Generation != 0 {
			return 0, fmt.Errorf("%w (it was deleted)", ErrSessionConflict)
		}
		next = 1
		_, err = tx.Exec(`INSERT INTO sessions
			(key, created_at, updated_at, metadata, last_consolidated, summary, generation)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sess.Key, unixNano(sess.CreatedAt), unixNano(sess.UpdatedAt), meta, sess.LastConsolidated, sess.Summary, next)
	case err != nil:
		return 0, err
	case stored != sess.Generation:
		return 0, fmt.Errorf("%w (stored generation %d, have %d)", ErrSessionConflict, stored, sess.Generation)
	default:
		next = stored + 1
		_, err = tx.Exec(`UPDATE sessions SET created_at = ?, updated_at = ?, metadata = ?,
			last_consolidated = ?, summary = ?, generation = ? WHERE key = ?`,
			unixNano(sess.CreatedAt), unixNano(sess.UpdatedAt), meta, sess.LastConsolidated, sess.Summary, next, sess.Key)
	}
	if err != nil {
		return 0, err
	}

	stmt, err := tx.Prepare(`INSERT INTO messages (session_key, position, record) VALUES (?, ?, ?)
		ON CONFLICT (session_key, position) DO UPDATE SET record = excluded.record
		WHERE messages.record != excluded.record`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for i, r := range records {
		if _, err := stmt.Exec(sess.Key, i, r); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_key = ? AND position >= ?`, sess.Key, len(records)); err != nil {
		return 0, err
	}
	return next, nil
}

// Delete removes the session and its messages. Deleting a missing session
// is not an error. A Session loaded before the delete can no longer be
// saved (ErrSessionConflict).
func (s *SQLiteStore) Delete(key string) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete session %q: %w", key, err)
	}
	return nil
}

// List returns the stored sessions, most recently updated first. Path is
// the database file for every session.
func (s *SQLiteStore) List() ([]Info, error) {
	rows, err := s.db.Query(`SELECT key, created_at, updated_at FROM sessions ORDER BY updated_at DESC, key`)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []Info
	for rows.Next() {
		var (
			info             Info
			created, updated int64
		)
		if err := rows.Scan(&info.Key, &created, &updated); err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		info.CreatedAt, info.UpdatedAt, info.Path = fromUnixNano(created), fromUnixNano(updated), s.path
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return out, nil
}

// encodeJSON encodes v compactly without HTML escaping, like the JSONL
// store, so equal values always produce equal records.
func encodeJSON(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// Timestamps are stored as Unix nanoseconds so they sort numerically; the
// zero time is stored as 0.
func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromUnixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}
