package session

import (
	"errors"
	"time"

	"github.com/yedamao/nanobot-go/internal/providers"
)

// ErrNotFound is returned when no session is stored under a key.
var ErrNotFound = errors.New("session not found")

// ErrSessionConflict is returned by Save when the stored session changed
// since sess was loaded or last saved (another process saved or deleted
// it), like nanobot's SessionConflictError. The save is not applied; load
// the session again to continue from the stored state.
var ErrSessionConflict = errors.New("session conflict: stored session changed since it was loaded")

// Store persists sessions. Implementations are safe for concurrent use;
// the *Session values they return are not.
type Store interface {
	// Load returns the session stored under key, or an error wrapping
	// ErrNotFound.
	Load(key string) (*Session, error)
	// Save stores sess, its full transcript and consolidation state.
	// Stores that detect concurrent writers fail with an error wrapping
	// ErrSessionConflict when sess.Generation is stale, and advance it on
	// success.
	Save(sess *Session) error
	// Delete removes the session stored under key. Deleting a missing
	// session is not an error.
	Delete(key string) error
	// List returns the stored sessions, most recently updated first.
	List() ([]Info, error)
	// Close releases the store's resources.
	Close() error
}

// Info summarizes a stored session.
type Info struct {
	Key       string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Path is the file holding the session: its JSONL file, or the
	// SQLite database shared by all sessions.
	Path string
}

func validRole(r providers.Role) bool {
	switch r {
	case providers.RoleSystem, providers.RoleUser, providers.RoleAssistant, providers.RoleTool:
		return true
	}
	return false
}
