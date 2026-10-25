package session

import (
	"errors"
	"sync"
)

// Manager caches sessions in memory on top of a Store. It is safe for
// concurrent use, but each *Session it returns must only be used by one
// goroutine at a time (the agent loop runs one worker per key).
type Manager struct {
	store Store

	mu    sync.Mutex
	cache map[string]*Session
}

// NewManager returns a manager storing sessions in store.
func NewManager(store Store) *Manager {
	return &Manager{store: store, cache: map[string]*Session{}}
}

// Store returns the underlying store.
func (m *Manager) Store() Store { return m.store }

// GetOrCreate returns the cached session for key, loading it from the
// store or creating an empty one (not yet saved) if needed.
func (m *Manager) GetOrCreate(key string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.cache[key]; ok {
		return s, nil
	}
	s, err := m.store.Load(key)
	if errors.Is(err, ErrNotFound) {
		s, err = New(key), nil
	}
	if err != nil {
		return nil, err
	}
	m.cache[key] = s
	return s, nil
}

// Load returns a fresh copy of the session stored under key, bypassing
// the cache, or an error wrapping ErrNotFound. Unlike GetOrCreate's
// cached *Session, which belongs to the agent loop's worker for key, the
// copy is the caller's own: it suits readers (history, chat lists) and
// edits saved back with Save, which then replaces the cached session.
func (m *Manager) Load(key string) (*Session, error) { return m.store.Load(key) }

// Save persists s and caches it. When the store reports a conflict, s is
// stale and dropped from the cache, so the next GetOrCreate loads the
// stored session instead of failing on every later save.
func (m *Manager) Save(s *Session) error {
	if err := m.store.Save(s); err != nil {
		if errors.Is(err, ErrSessionConflict) {
			m.mu.Lock()
			if m.cache[s.Key] == s {
				delete(m.cache, s.Key)
			}
			m.mu.Unlock()
		}
		return err
	}
	m.mu.Lock()
	m.cache[s.Key] = s
	m.mu.Unlock()
	return nil
}

// Delete removes key from the cache and from the store.
func (m *Manager) Delete(key string) error {
	m.mu.Lock()
	delete(m.cache, key)
	m.mu.Unlock()
	return m.store.Delete(key)
}

// List returns the stored sessions, most recently updated first.
func (m *Manager) List() ([]Info, error) { return m.store.List() }

// Close closes the underlying store.
func (m *Manager) Close() error { return m.store.Close() }
