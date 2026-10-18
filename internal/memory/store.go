// Package memory ports stage 1 of nanobot's memory (nanobot/agent/memory.py):
// a Store for the append-only <workspace>/memory/history.jsonl archive and
// a Consolidator that summarizes the oldest unconsolidated session messages
// into it when the replayed history grows too large.
package memory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yedamao/nanobot-go/internal/fsutil"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// Workspace-relative locations of the memory files. The shell guard denies
// writes to HistoryFile and DreamCursorFile: only the Store appends to
// them.
const (
	Dir             = "memory"
	HistoryFile     = "history.jsonl"
	DreamCursorFile = ".dream_cursor"
)

// maxEntryChars is an emergency cap on one history entry, like nanobot's
// _HISTORY_ENTRY_HARD_CAP. Callers should bound their content well below
// it; it catches a model echoing its whole input back as a "summary".
const maxEntryChars = 64000

// timestampLayout matches nanobot's "%Y-%m-%d %H:%M".
const timestampLayout = "2006-01-02 15:04"

// Entry is one line of history.jsonl.
type Entry struct {
	// Cursor increases by one per entry; Dream (phase 2) remembers the
	// last cursor it processed.
	Cursor     int    `json:"cursor"`
	Timestamp  string `json:"timestamp"`
	Content    string `json:"content"`
	SessionKey string `json:"session_key,omitempty"`
}

// Store appends to and reads history.jsonl. It is safe for concurrent use
// within one process; appends from several processes sharing a workspace
// may allocate the same cursor.
type Store struct {
	dir string
	now func() time.Time

	mu sync.Mutex
}

// NewStore returns a store for the memory directory of workspace. The
// directory is created on the first append.
func NewStore(workspace string) *Store {
	return &Store{dir: filepath.Join(workspace, Dir), now: time.Now}
}

// HistoryPath returns the path of history.jsonl.
func (s *Store) HistoryPath() string { return filepath.Join(s.dir, HistoryFile) }

// Append adds content as a new entry and returns it. The entry is written
// with a single append and fsynced before Append returns, so a nil error
// means the entry is durable. A failed write is truncated away, and a torn
// last line left by an earlier crash is terminated first, so every line
// stays one complete JSON object.
func (s *Store) Append(content, sessionKey string) (Entry, error) {
	content = tools.Truncate(strings.TrimRight(content, " \t\r\n"), maxEntryChars)
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.HistoryPath()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Entry{}, fmt.Errorf("append history: %w", err)
	}
	f, err := openRegular(path, os.O_RDWR|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return Entry{}, fmt.Errorf("append history: %w", err)
	}
	defer f.Close()

	last, size, torn, err := scan(f)
	if err != nil {
		return Entry{}, fmt.Errorf("append history: %w", err)
	}
	e := Entry{Cursor: last + 1, Timestamp: s.now().Format(timestampLayout), Content: content, SessionKey: sessionKey}
	var buf bytes.Buffer
	if torn {
		buf.WriteByte('\n')
	}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return Entry{}, fmt.Errorf("append history: %w", err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Truncate(size) // drop a partial line
		return Entry{}, fmt.Errorf("append history: %w", err)
	}
	if err := f.Sync(); err != nil {
		return Entry{}, fmt.Errorf("append history: %w", err)
	}
	return e, nil
}

// Entries returns every valid entry in file order. Lines that are not
// valid JSON objects with a positive cursor are skipped, so one corrupt
// line cannot hide the rest. A missing file yields no entries.
func (s *Store) Entries() ([]Entry, error) {
	f, err := openRegular(s.HistoryPath(), os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	defer f.Close()
	var out []Entry
	err = eachLine(f, func(line []byte) {
		if e, ok := parseEntry(line); ok {
			out = append(out, e)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	return out, nil
}

// openRegular refuses anything but a regular file: the agent can create
// files in the workspace, and a FIFO, device or symlink planted at
// history.jsonl must not hang or redirect the archive.
func openRegular(path string, flag int) (*os.File, error) {
	return fsutil.OpenRegularNoFollow(path, flag, 0o600)
}

// scan reads f from the start and returns the highest valid cursor, the
// file size, and whether the last line lacks its newline.
func scan(f *os.File) (last int, size int64, torn bool, err error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, 0, false, err
	}
	var lastByte byte
	err = eachLine(f, func(line []byte) {
		size += int64(len(line))
		lastByte = line[len(line)-1]
		if e, ok := parseEntry(line); ok && e.Cursor > last {
			last = e.Cursor
		}
	})
	return last, size, size > 0 && lastByte != '\n', err
}

// eachLine calls fn with every non-empty raw line of r, newline included.
// It does not cap line length: entries are bounded by maxEntryChars.
func eachLine(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			fn(line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func parseEntry(line []byte) (Entry, bool) {
	var e Entry
	if json.Unmarshal(bytes.TrimSpace(line), &e) != nil || e.Cursor <= 0 {
		return Entry{}, false
	}
	return e, true
}
