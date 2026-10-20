package cron

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/yedamao/nanobot-go/internal/fsutil"
)

// Workspace-relative location of the job store, like nanobot's.
const (
	Dir      = "cron"
	JobsFile = "jobs.json"
	// lockFile serializes store updates across processes.
	lockFile = ".jobs.lock"
	// storeVersion is the jobs.json format version.
	storeVersion = 1
)

// storeFile is jobs.json.
type storeFile struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"`
}

// Store persists jobs in <workspace>/cron/jobs.json. Every Update is a
// read-modify-write under a mutex and an advisory file lock, and the
// file is replaced atomically, so the gateway, `nanobot agent` and
// `nanobot cron` can change the jobs concurrently without losing an
// update. (Where flock is unavailable only the mutex applies, so other
// processes' writes may be lost there.) Nothing is cached: every call
// reads the file.
//
// A file that does not parse is an error, never treated as empty, so a
// corrupt store is not overwritten by the next update; fix or remove it
// by hand (nanobot renames it aside instead).
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore returns the store of the workspace at root. Nothing is
// created until the first update.
func NewStore(root string) *Store {
	return &Store{dir: filepath.Join(root, Dir)}
}

// Path returns the jobs.json path.
func (s *Store) Path() string { return filepath.Join(s.dir, JobsFile) }

// Load returns the jobs in file order; a missing file holds none.
func (s *Store) Load() ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

// Update runs fn on the current jobs and saves what it returns, holding
// the lock throughout. When fn fails nothing is written.
func (s *Store) Update(fn func(jobs []Job) ([]Job, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	jobs, err := s.read()
	if err != nil {
		return err
	}
	jobs, err = fn(jobs)
	if err != nil {
		return err
	}
	return s.write(jobs)
}

func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("cron store: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cron store: %w", err)
	}
	if err := fsutil.LockExclusive(f, true); err != nil {
		f.Close()
		return nil, fmt.Errorf("cron store: lock %s: %w", f.Name(), err)
	}
	return func() { f.Close() }, nil
}

func (s *Store) read() ([]Job, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cron store: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("cron store %s is corrupt (fix or remove it; it will not be overwritten): %w", s.Path(), err)
	}
	if f.Version > storeVersion {
		return nil, fmt.Errorf("cron store %s has version %d; this nanobot-go understands up to %d", s.Path(), f.Version, storeVersion)
	}
	return f.Jobs, nil
}

func (s *Store) write(jobs []Job) error {
	if jobs == nil {
		jobs = []Job{}
	}
	data, err := json.MarshalIndent(storeFile{Version: storeVersion, Jobs: jobs}, "", "  ")
	if err != nil {
		return fmt.Errorf("cron store: %w", err)
	}
	if err := fsutil.WriteFileAtomic(s.Path(), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("cron store: %w", err)
	}
	return nil
}

// find returns the index of the job with id, or -1.
func find(jobs []Job, id string) int {
	return slices.IndexFunc(jobs, func(j Job) bool { return j.ID == id })
}
