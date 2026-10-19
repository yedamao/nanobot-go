package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// WorkspaceID returns a stable identifier for a workspace directory: 32
// hex digits of the SHA-256 of its absolute path, with symlinks resolved
// when the path exists. nanobot stores a random ID in the workspace
// instead; deriving it from the path keeps the workspace untouched, at the
// cost of a new (empty) session database when the workspace moves.
func WorkspaceID(workspace string) (string, error) {
	p, err := canonicalPath(workspace)
	if err != nil {
		return "", fmt.Errorf("workspace id: %w", err)
	}
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:16]), nil
}

// SQLitePath returns where the session database of workspace lives:
// <sessionsDir>/<WorkspaceID>/sessions.sqlite3. Like nanobot it refuses a
// location inside the workspace, where the agent's file tools could edit
// its own history.
func SQLitePath(sessionsDir, workspace string) (string, error) {
	id, err := WorkspaceID(workspace)
	if err != nil {
		return "", err
	}
	ws, err := canonicalPath(workspace)
	if err != nil {
		return "", err
	}
	dir, err := canonicalPath(filepath.Join(sessionsDir, id))
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(ws, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("session storage %s must be outside the workspace %s; move the config directory or the workspace", dir, ws)
	}
	return filepath.Join(dir, SQLiteFileName), nil
}

// Open opens the session store of workspace: the SQLite database at
// SQLitePath(sessionsDir, workspace). The flat JSONL session files in
// sessionsDir are imported into it the first time it is opened.
func Open(sessionsDir, workspace string) (Store, error) {
	path, err := SQLitePath(sessionsDir, workspace)
	if err != nil {
		return nil, fmt.Errorf("open sessions: %w", err)
	}
	s, err := OpenSQLite(path, sessionsDir)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// canonicalPath makes p absolute and resolves symlinks in its longest
// existing prefix, so a path compares equal however it was spelled.
func canonicalPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var rest []string
	for cur := abs; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(append([]string{resolved}, rest...)...), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
}
