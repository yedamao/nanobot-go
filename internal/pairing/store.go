// Package pairing keeps the chat senders approved through pairing codes.
// It mirrors nanobot/pairing/store.py: when a sender a channel does not
// allow writes to the bot in a private chat, the channel replies with a
// short-lived pairing code; the owner approves the code on the host
// (`nanobot pairing approve <code>`), which adds the sender to the
// channel's approved list.
package pairing

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yedamao/nanobot-go/internal/fsutil"
)

const (
	// FileName is the store's file, next to the config file.
	FileName = "pairing.json"
	// lockFile serializes store updates across processes.
	lockFile = ".pairing.lock"
	// DefaultTTL is how long a pairing code stays valid (nanobot:
	// _TTL_DEFAULT_S).
	DefaultTTL = 10 * time.Minute
	// alphabet and codeLength shape codes like ABCD-EFGH.
	alphabet   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	codeLength = 8
)

// Request is a pending pairing request.
type Request struct {
	Code      string
	Channel   string
	SenderID  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// storeFile is pairing.json, in nanobot's layout: approved sender IDs
// per channel and pending requests by code, with times in Unix seconds.
type storeFile struct {
	Approved map[string][]string    `json:"approved"`
	Pending  map[string]pendingJSON `json:"pending"`
}

type pendingJSON struct {
	Channel   string  `json:"channel"`
	SenderID  string  `json:"sender_id"`
	CreatedAt float64 `json:"created_at"`
	ExpiresAt float64 `json:"expires_at"`
}

// Store persists approvals and pending codes in one JSON file. Every
// change is a read-modify-write under a mutex and an advisory file lock,
// and the file is replaced atomically, so the gateway and `nanobot
// pairing` can use it concurrently. Nothing is cached: every call reads
// the file.
//
// A file that does not parse is an error, never treated as empty, so a
// corrupt store cannot silently drop every approval on the next write
// (nanobot resets it instead). Lookups fail closed on it.
type Store struct {
	path string
	ttl  time.Duration
	now  func() time.Time
	rand io.Reader
	mu   sync.Mutex
}

// NewStore returns the store at path. Nothing is created until the first
// change.
func NewStore(path string) *Store {
	return &Store{path: path, ttl: DefaultTTL, now: time.Now, rand: rand.Reader}
}

// PathFor returns the store path for a config file: pairing.json next to
// it, like the sessions directory.
func PathFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), FileName)
}

// Path returns the store's file.
func (s *Store) Path() string { return s.path }

// Code returns the active pairing code of senderID on channel, creating
// one if there is none, so a sender who writes again gets the same code.
func (s *Store) Code(channel, senderID string) (string, error) {
	var code string
	err := s.update(func(f *storeFile) (bool, error) {
		for c, p := range f.Pending {
			if p.Channel == channel && p.SenderID == senderID {
				code = c
				return false, nil
			}
		}
		for {
			c, err := s.newCode()
			if err != nil {
				return false, err
			}
			if _, taken := f.Pending[c]; !taken {
				code = c
				break
			}
		}
		now := s.now()
		f.Pending[code] = pendingJSON{Channel: channel, SenderID: senderID,
			CreatedAt: unixSeconds(now), ExpiresAt: unixSeconds(now.Add(s.ttl))}
		return true, nil
	})
	return code, err
}

// Approve approves a pending code and returns its request. ok is false
// when the code is unknown or expired. Codes match case-insensitively,
// with or without the dash.
func (s *Store) Approve(code string) (req Request, ok bool, err error) {
	code = NormalizeCode(code)
	err = s.update(func(f *storeFile) (bool, error) {
		p, found := f.Pending[code]
		if !found {
			return false, nil
		}
		delete(f.Pending, code)
		if !slices.Contains(f.Approved[p.Channel], p.SenderID) {
			f.Approved[p.Channel] = append(f.Approved[p.Channel], p.SenderID)
			slices.Sort(f.Approved[p.Channel])
		}
		req, ok = p.request(code), true
		return true, nil
	})
	return req, ok, err
}

// Deny discards a pending code. It reports whether the code existed.
func (s *Store) Deny(code string) (bool, error) {
	code = NormalizeCode(code)
	var found bool
	err := s.update(func(f *storeFile) (bool, error) {
		_, found = f.Pending[code]
		delete(f.Pending, code)
		return found, nil
	})
	return found, err
}

// Revoke removes an approved sender. It reports whether the sender was
// approved.
func (s *Store) Revoke(channel, senderID string) (bool, error) {
	var found bool
	err := s.update(func(f *storeFile) (bool, error) {
		ids := f.Approved[channel]
		i := slices.Index(ids, senderID)
		if found = i >= 0; !found {
			return false, nil
		}
		if ids = slices.Delete(ids, i, i+1); len(ids) == 0 {
			delete(f.Approved, channel)
		} else {
			f.Approved[channel] = ids
		}
		return true, nil
	})
	return found, err
}

// IsApproved reports whether senderID was approved on channel.
func (s *Store) IsApproved(channel, senderID string) (bool, error) {
	f, err := s.load()
	if err != nil {
		return false, err
	}
	return slices.Contains(f.Approved[channel], senderID), nil
}

// Pending returns the unexpired requests, oldest first.
func (s *Store) Pending() ([]Request, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	now := unixSeconds(s.now())
	var out []Request
	for code, p := range f.Pending {
		if p.live(now) {
			out = append(out, p.request(code))
		}
	}
	slices.SortFunc(out, func(a, b Request) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Code, b.Code)
	})
	return out, nil
}

// Approved returns the approved sender IDs by channel.
func (s *Store) Approved() (map[string][]string, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.Approved, nil
}

// Reply is the message sent to an unknown private-chat sender (nanobot:
// format_pairing_reply, pointing at the CLI instead of the WebUI). It is
// plain text.
func Reply(code string) string {
	return "Hi! This is a private nanobot.\n\n" +
		"Your pairing code is: " + code + "\n\n" +
		"Ask the owner to approve it on the host with:\n" +
		"nanobot pairing approve " + code + "\n\n" +
		fmt.Sprintf("The code expires in %d minutes.", int(DefaultTTL.Minutes()))
}

// NormalizeCode upper-cases code and restores the dash, so "abcdefgh"
// and "abcd-efgh" both match ABCD-EFGH.
func NormalizeCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) == codeLength && !strings.Contains(code, "-") {
		code = code[:codeLength/2] + "-" + code[codeLength/2:]
	}
	return code
}

func (s *Store) newCode() (string, error) {
	// Rejection sampling keeps every character equally likely.
	limit := byte(256 - 256%len(alphabet))
	var b strings.Builder
	buf := make([]byte, 1)
	for b.Len() < codeLength+1 {
		if b.Len() == codeLength/2 {
			b.WriteByte('-')
			continue
		}
		if _, err := io.ReadFull(s.rand, buf); err != nil {
			return "", fmt.Errorf("pairing: generate code: %w", err)
		}
		if buf[0] < limit {
			b.WriteByte(alphabet[int(buf[0])%len(alphabet)])
		}
	}
	return b.String(), nil
}

// load reads the store without the file lock (writes are atomic
// renames) and drops expired requests from the result.
func (s *Store) load() (*storeFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	gc(f, unixSeconds(s.now()))
	return f, nil
}

// update runs fn on the store (with expired requests dropped) and saves
// it when fn reports a change or expired requests were dropped.
func (s *Store) update(fn func(*storeFile) (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	dropped := gc(f, unixSeconds(s.now()))
	changed, err := fn(f)
	if err != nil {
		return err
	}
	if !changed && !dropped {
		return nil
	}
	return s.write(f)
}

func (s *Store) lock() (func(), error) {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pairing store: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("pairing store: %w", err)
	}
	if err := fsutil.LockExclusive(f, true); err != nil {
		f.Close()
		return nil, fmt.Errorf("pairing store: lock %s: %w", f.Name(), err)
	}
	return func() { f.Close() }, nil
}

func (s *Store) read() (*storeFile, error) {
	f := &storeFile{Approved: map[string][]string{}, Pending: map[string]pendingJSON{}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pairing store: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("pairing store %s is corrupt (fix or remove it; it will not be overwritten): %w", s.path, err)
	}
	if f.Approved == nil {
		f.Approved = map[string][]string{}
	}
	if f.Pending == nil {
		f.Pending = map[string]pendingJSON{}
	}
	return f, nil
}

func (s *Store) write(f *storeFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("pairing store: %w", err)
	}
	if err := fsutil.WriteFileAtomic(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("pairing store: %w", err)
	}
	return nil
}

// gc drops expired and malformed requests. It reports whether it dropped
// any.
func gc(f *storeFile, now float64) bool {
	n := len(f.Pending)
	maps.DeleteFunc(f.Pending, func(_ string, p pendingJSON) bool { return !p.live(now) })
	return len(f.Pending) != n
}

func (p pendingJSON) live(now float64) bool {
	return p.Channel != "" && p.SenderID != "" && p.ExpiresAt >= now
}

func (p pendingJSON) request(code string) Request {
	return Request{Code: code, Channel: p.Channel, SenderID: p.SenderID,
		CreatedAt: fromUnixSeconds(p.CreatedAt), ExpiresAt: fromUnixSeconds(p.ExpiresAt)}
}

func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func fromUnixSeconds(s float64) time.Time {
	sec, frac := math.Modf(s)
	return time.Unix(int64(sec), int64(frac*1e9))
}
