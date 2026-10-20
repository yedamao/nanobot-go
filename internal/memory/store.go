// Package memory ports nanobot's two-stage memory (nanobot/agent/memory.py).
// Stage 1: a Store for the append-only <workspace>/memory/history.jsonl
// archive and a Consolidator that summarizes the oldest unconsolidated
// session messages into it when the replayed history grows too large.
// Stage 2: Dream, which folds archived entries into the long-term files
// (SOUL.md, USER.md, memory/MEMORY.md, skills/) through an ephemeral agent
// turn with a restricted tool registry, commits them to git and trims the
// archive.
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yedamao/nanobot-go/internal/fsutil"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// Workspace-relative locations of the memory files. The shell guard denies
// writes to HistoryFile and DreamCursorFile: only the Store writes them.
// LongTermFile (memory/MEMORY.md) is curated by Dream.
const (
	Dir             = "memory"
	HistoryFile     = "history.jsonl"
	DreamCursorFile = ".dream_cursor"
	LongTermFile    = "MEMORY.md"
	// lockFile serializes history writers across processes.
	lockFile = ".history.lock"
)

// errLocked is returned by a non-waiting lock attempt on a held lock.
var errLocked = fsutil.ErrLocked

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

// Store appends to, reads and compacts history.jsonl and keeps Dream's
// cursor. It is safe for concurrent use. Appends and compaction also take
// an advisory lock on memory/.history.lock (on Unix), so separate
// processes sharing a workspace, such as an agent and a scheduled
// `nanobot dream`, neither allocate the same cursor nor lose an append to
// a compaction.
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
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Entry{}, fmt.Errorf("append history: %w", err)
	}
	unlock, err := s.lock()
	if err != nil {
		return Entry{}, fmt.Errorf("append history: %w", err)
	}
	defer unlock()

	f, err := openRegular(s.HistoryPath(), os.O_RDWR|os.O_CREATE|os.O_APPEND)
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

// DreamCursorPath returns the path of .dream_cursor.
func (s *Store) DreamCursorPath() string { return filepath.Join(s.dir, DreamCursorFile) }

// DreamCursor returns the cursor of the last history entry Dream has
// processed, or 0 when Dream has never completed a run. A cursor file that
// does not hold a non-negative integer is an error rather than 0: guessing
// would either replay the whole archive or let compaction drop entries
// Dream never saw.
func (s *Store) DreamCursor() (int, error) {
	f, err := openRegular(s.DreamCursorPath(), os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read dream cursor: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64))
	if err != nil {
		return 0, fmt.Errorf("read dream cursor: %w", err)
	}
	text := strings.TrimSpace(string(data))
	c, err := strconv.Atoi(text)
	if err != nil || c < 0 {
		return 0, fmt.Errorf("dream cursor %s is corrupt (%q); fix or delete it", s.DreamCursorPath(), text)
	}
	return c, nil
}

// SetDreamCursor records that Dream has processed every entry up to and
// including cursor. The file is replaced atomically.
func (s *Store) SetDreamCursor(cursor int) error {
	if cursor < 0 {
		return fmt.Errorf("set dream cursor: negative cursor %d", cursor)
	}
	return fsutil.WriteFileAtomic(s.DreamCursorPath(), []byte(strconv.Itoa(cursor)+"\n"), 0o600)
}

// Compact trims history.jsonl to the newest maxEntries valid entries, like
// nanobot's compact_history, but never drops an entry newer than the
// Dream cursor: those are kept even beyond the limit. It returns the
// number of entries dropped. maxEntries <= 0 disables compaction. The file
// is rewritten atomically (corrupt lines are not carried over) and only
// when there is something to drop.
func (s *Store) Compact(maxEntries int) (int, error) {
	if maxEntries <= 0 {
		return 0, nil
	}
	unlock, err := s.lock()
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil // no memory directory: nothing archived yet
	}
	if err != nil {
		return 0, fmt.Errorf("compact history: %w", err)
	}
	defer unlock()

	entries, err := s.Entries()
	if err != nil || len(entries) <= maxEntries {
		return 0, err
	}
	// Read under the lock so the cursor is at least as new as the entries.
	processed, err := s.DreamCursor()
	if err != nil {
		return 0, fmt.Errorf("compact history: %w", err)
	}
	firstPending := len(entries)
	for i, e := range entries {
		if e.Cursor > processed {
			firstPending = i
			break
		}
	}
	drop := min(len(entries)-maxEntries, firstPending)
	if drop <= 0 {
		return 0, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, e := range entries[drop:] {
		if err := enc.Encode(e); err != nil {
			return 0, fmt.Errorf("compact history: %w", err)
		}
	}
	if err := fsutil.WriteFileAtomic(s.HistoryPath(), buf.Bytes(), 0o600); err != nil {
		return 0, fmt.Errorf("compact history: %w", err)
	}
	return drop, nil
}

// lock takes the in-process mutex and then the advisory file lock that
// serializes history writers across processes. The memory directory must
// exist.
func (s *Store) lock() (unlock func(), err error) {
	s.mu.Lock()
	f, err := openRegular(filepath.Join(s.dir, lockFile), os.O_RDWR|os.O_CREATE)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := fsutil.LockExclusive(f, true); err != nil {
		f.Close()
		s.mu.Unlock()
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return func() {
		f.Close() // releases the lock
		s.mu.Unlock()
	}, nil
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
