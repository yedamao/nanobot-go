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
	// LastConsolidated and LastSummary hold the consolidation state; see
	// Session.
	LastConsolidated int    `json:"last_consolidated,omitempty"`
	LastSummary      string `json:"last_summary,omitempty"`
}

// JSONLStore reads and writes session files in one directory:
// <dir>/<encoded key>.jsonl, a metadata line followed by one message per
// line. Every save rewrites the file atomically. It does not detect
// concurrent writers: Session.Generation is ignored and the last save
// wins.
type JSONLStore struct {
	dir string
}

var _ Store = (*JSONLStore)(nil)

// NewJSONLStore returns a store rooted at dir. The directory is created
// on the first save.
func NewJSONLStore(dir string) *JSONLStore { return &JSONLStore{dir: dir} }

// Dir returns the store's directory.
func (s *JSONLStore) Dir() string { return s.dir }

// Path returns the file that holds key.
func (s *JSONLStore) Path(key string) string {
	return filepath.Join(s.dir, fileName(key))
}

// Load reads the session stored under key. It returns ErrNotFound when
// there is none. Message lines that are not valid JSON are skipped so one
// corrupt line cannot make a conversation unreadable.
func (s *JSONLStore) Load(key string) (*Session, error) {
	sess, err := loadJSONLFile(s.Path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("load session %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load session %q: %w", key, err)
	}
	if sess.Key != key {
		return nil, fmt.Errorf("load session %q: file holds session %q", key, sess.Key)
	}
	return sess, nil
}

// loadJSONLFile reads one session file; the key comes from its metadata
// line.
func loadJSONLFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	meta, err := readMetadata(r)
	if err != nil {
		return nil, err
	}
	sess := &Session{Key: meta.Key, CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt, Metadata: meta.Metadata}
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
			return nil, err
		}
	}
	sess.setConsolidation(meta.LastConsolidated, meta.LastSummary)
	return sess, nil
}

// Save writes sess atomically: a crash leaves either the previous file or
// the new one, never a partial file.
func (s *JSONLStore) Save(sess *Session) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	meta := metadataLine{
		Type: metadataType, Key: sess.Key, CreatedAt: sess.CreatedAt, UpdatedAt: sess.UpdatedAt, Metadata: sess.Metadata,
		LastConsolidated: sess.LastConsolidated, LastSummary: sess.Summary,
	}
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
func (s *JSONLStore) Delete(key string) error {
	if err := os.Remove(s.Path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete session %q: %w", key, err)
	}
	return nil
}

// List returns stored sessions, most recently updated first. Unreadable
// files are skipped.
func (s *JSONLStore) List() ([]Info, error) {
	names, err := s.files()
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	var out []Info
	for _, name := range names {
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

// Close does nothing; it exists to satisfy Store.
func (s *JSONLStore) Close() error { return nil }

// files returns the names of the session files in the store's directory,
// sorted. Hidden files, directories and other non-regular files are
// skipped. A missing directory holds no files.
func (s *JSONLStore) files() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, fileExt) || strings.HasPrefix(name, ".") {
			continue
		}
		names = append(names, name)
	}
	return names, nil
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

// fileName maps a session key to a safe, unique file name. Lower-case
// letters, digits, '-' and '.' are kept, ':' becomes '_' (so "cli:direct"
// is "cli_direct.jsonl"), and every other byte, including '_' and
// upper-case letters, is percent-encoded. That keeps the mapping injective
// even on case-insensitive filesystems, where "cli:Work" and "cli:work"
// would otherwise share a file. A leading '.' is encoded so files are
// never hidden or "..", and the empty key gets the name "%", which no
// other key produces. Long names are cut and suffixed with a hash of the
// key.
func fileName(key string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.' && i > 0:
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
		name = "%"
	}
	if len(name) > maxFileNameBytes {
		sum := sha256.Sum256([]byte(key))
		name = name[:maxFileNameBytes-17] + "~" + hex.EncodeToString(sum[:8])
	}
	return name + fileExt
}
