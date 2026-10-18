package session

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/fsutil"
	"github.com/yedamao/nanobot-go/internal/providers"
)

// ErrNotFound is returned when a session file does not exist.
var ErrNotFound = errors.New("session not found")

const (
	fileExt = ".jsonl"
	// metadataType tags the first line of a session file.
	metadataType = "metadata"
	// maxFileNameBytes keeps encoded names well under filesystem limits.
	maxFileNameBytes = 200
)

// metadataLine is the first line of a session file; message lines follow.
type metadataLine struct {
	Type      string         `json:"_type"`
	Key       string         `json:"key"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Info summarizes a stored session.
type Info struct {
	Key       string
	CreatedAt time.Time
	UpdatedAt time.Time
	Path      string
}

// Store reads and writes session files in one directory:
// <dir>/<encoded key>.jsonl, a metadata line followed by one message per
// line. Every save rewrites the file atomically.
type Store struct {
	dir string
}

// NewStore returns a store rooted at dir. The directory is created on the
// first save.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Dir returns the store's directory.
func (s *Store) Dir() string { return s.dir }

// Path returns the file that holds key.
func (s *Store) Path(key string) string {
	return filepath.Join(s.dir, fileName(key))
}

// Load reads the session stored under key. It returns ErrNotFound when
// there is none. Message lines that are not valid JSON are skipped so one
// corrupt line cannot make a conversation unreadable.
func (s *Store) Load(key string) (*Session, error) {
	f, err := os.Open(s.Path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("load session %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load session %q: %w", key, err)
	}
	defer f.Close()

	r := bufio.NewReader(f)
	meta, err := readMetadata(r)
	if err != nil {
		return nil, fmt.Errorf("load session %q: %w", key, err)
	}
	if meta.Key != key {
		return nil, fmt.Errorf("load session %q: file holds session %q", key, meta.Key)
	}
	sess := &Session{Key: key, CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt, Metadata: meta.Metadata}
	if sess.Metadata == nil {
		sess.Metadata = map[string]any{}
	}
	for {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			var m providers.Message
			if json.Unmarshal(line, &m) == nil && validRole(m.Role) {
				sess.Messages = append(sess.Messages, m)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("load session %q: %w", key, err)
		}
	}
	return sess, nil
}

// Save writes sess atomically: a crash leaves either the previous file or
// the new one, never a partial file.
func (s *Store) Save(sess *Session) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	meta := metadataLine{Type: metadataType, Key: sess.Key, CreatedAt: sess.CreatedAt, UpdatedAt: sess.UpdatedAt, Metadata: sess.Metadata}
	if err := enc.Encode(meta); err != nil {
		return fmt.Errorf("save session %q: %w", sess.Key, err)
	}
	for _, m := range sess.Messages {
		if err := enc.Encode(m); err != nil {
			return fmt.Errorf("save session %q: %w", sess.Key, err)
		}
	}
	if err := fsutil.WriteFileAtomic(s.Path(sess.Key), buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("save session %q: %w", sess.Key, err)
	}
	return nil
}

// Delete removes the session stored under key. Deleting a missing session
// is not an error.
func (s *Store) Delete(key string) error {
	if err := os.Remove(s.Path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete session %q: %w", key, err)
	}
	return nil
}

// List returns stored sessions, most recently updated first. Unreadable
// files are skipped.
func (s *Store) List() ([]Info, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	var out []Info
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, fileExt) || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(s.dir, name)
		meta, err := readMetadataFile(path)
		if err != nil {
			continue
		}
		out = append(out, Info{Key: meta.Key, CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt, Path: path})
	}
	slices.SortFunc(out, func(a, b Info) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return out, nil
}

func readMetadataFile(path string) (metadataLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return metadataLine{}, err
	}
	defer f.Close()
	return readMetadata(bufio.NewReader(f))
}

func readMetadata(r *bufio.Reader) (metadataLine, error) {
	line, err := r.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return metadataLine{}, err
	}
	var meta metadataLine
	if err := json.Unmarshal(line, &meta); err != nil {
		return metadataLine{}, fmt.Errorf("bad metadata line: %w", err)
	}
	if meta.Type != metadataType {
		return metadataLine{}, fmt.Errorf("bad metadata line: _type is %q", meta.Type)
	}
	return meta, nil
}

func validRole(r providers.Role) bool {
	switch r {
	case providers.RoleSystem, providers.RoleUser, providers.RoleAssistant, providers.RoleTool:
		return true
	}
	return false
}

// fileName maps a session key to a safe, unique file name. Letters,
// digits, '-' and '.' are kept, ':' becomes '_' (so "cli:direct" is
// "cli_direct.jsonl"), and every other byte, including '_' itself, is
// percent-encoded, which keeps the mapping injective. A leading '.' is
// encoded so files are never hidden or "..". Long names are cut and
// suffixed with a hash of the key.
func fileName(key string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.' && i > 0:
			b.WriteByte(c)
		case c == ':':
			b.WriteByte('_')
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xF])
		}
	}
	name := b.String()
	if name == "" {
		name = "%00"
	}
	if len(name) > maxFileNameBytes {
		sum := sha256.Sum256([]byte(key))
		name = name[:maxFileNameBytes-17] + "~" + hex.EncodeToString(sum[:8])
	}
	return name + fileExt
}
