package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/yedamao/nanobot-go/internal/fsutil"
)

// The credential store ports nanobot/agent/tools/mcp_oauth.py's
// MCPOAuthStorage: one JSON file, <data dir>/auth/mcp.json, holding each
// configured server's tokens, registered client and authorization server
// metadata. Entries are bound to the config name and a fingerprint of the
// server URL, so a renamed or re-pointed server never sees another's
// tokens. Every change happens under a file lock and replaces the file
// atomically.

const (
	oauthStoreVersion      = 1
	oauthStoreLockTimeout  = 15 * time.Second
	oauthRefreshLockTimout = 60 * time.Second
	// maxOAuthStore bounds the credential file read into memory.
	maxOAuthStore = 4 << 20
)

// ErrAuthorizationRequired reports that an OAuth MCP server has no usable
// credentials and needs a browser sign-in from the WebUI. Background
// connections never start that sign-in themselves.
var ErrAuthorizationRequired = errors.New("MCP server requires browser authorization")

// OAuthStore is the MCP OAuth credential file. It is safe for concurrent
// use, also across processes sharing the data directory (flock).
type OAuthStore struct {
	path   string
	logger *slog.Logger
	mu     sync.Mutex // serializes this process; the file lock the others
}

// NewOAuthStore returns the store at path (see config.MCPOAuthPath).
// Nothing is created until the first write.
func NewOAuthStore(path string, logger *slog.Logger) *OAuthStore {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &OAuthStore{path: path, logger: logger}
}

// storeFile is the file's shape, as nanobot writes it.
type storeFile struct {
	Version     int                      `json:"version"`
	Servers     map[string]*storedServer `json:"servers"`
	Generations map[string]string        `json:"generations"`
}

type storedServer struct {
	ServerFingerprint string          `json:"server_fingerprint,omitempty"`
	WriteLease        string          `json:"write_lease,omitempty"`
	Tokens            *storedTokens   `json:"tokens,omitempty"`
	ExpiresAt         *float64        `json:"expires_at,omitempty"`
	TokenIssuer       string          `json:"token_issuer,omitempty"`
	ClientInfo        *storedClient   `json:"client_info,omitempty"`
	OAuthMetadata     *storedMetadata `json:"oauth_metadata,omitempty"`
	OAuthIssuer       string          `json:"oauth_issuer,omitempty"`
	RedirectURI       string          `json:"redirect_uri,omitempty"`
}

// storedTokens is an OAuth token response (RFC 6749 section 5.1).
type storedTokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
	Scope        string `json:"scope,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// storedClient is the client registered with the authorization server.
type storedClient struct {
	ClientID                string   `json:"client_id"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	RedirectURIs            []string `json:"redirect_uris,omitempty"`
}

// storedMetadata is the part of the authorization server metadata
// (RFC 8414) needed to refresh tokens without rediscovery.
type storedMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint,omitempty"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint,omitempty"`
	ScopesSupported                   []string `json:"scopes_supported,omitempty"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported,omitempty"`
}

func emptyStore() *storeFile {
	return &storeFile{Version: oauthStoreVersion, Servers: map[string]*storedServer{}, Generations: map[string]string{}}
}

// serverFingerprint is nanobot's: SHA-256 of the trimmed server URL.
func serverFingerprint(serverURL string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(serverURL)))
	return hex.EncodeToString(sum[:])
}

func normalizeIssuer(s string) string { return strings.TrimRight(s, "/") }

// newLease returns a random token like Python's secrets.token_urlsafe(24).
func newLease() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// readUnlocked reads the file, tolerating a missing or damaged one (and
// dropping damaged entries) as nanobot does.
func (s *OAuthStore) readUnlocked() *storeFile {
	data, err := readCapped(s.path, maxOAuthStore)
	if errors.Is(err, os.ErrNotExist) {
		return emptyStore()
	}
	if err != nil {
		s.logger.Warn("could not read MCP OAuth credentials", "error", err)
		return emptyStore()
	}
	var raw struct {
		Servers     map[string]json.RawMessage `json:"servers"`
		Generations map[string]json.RawMessage `json:"generations"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || raw.Servers == nil {
		if err != nil {
			s.logger.Warn("could not read MCP OAuth credentials", "error", "invalid JSON")
		}
		return emptyStore()
	}
	out := emptyStore()
	for name, v := range raw.Servers {
		var e storedServer
		if json.Unmarshal(v, &e) != nil {
			continue
		}
		if e.ExpiresAt != nil && (math.IsNaN(*e.ExpiresAt) || math.IsInf(*e.ExpiresAt, 0)) {
			e.ExpiresAt = nil
		}
		e.OAuthIssuer = normalizeIssuer(e.OAuthIssuer)
		out.Servers[name] = &e
	}
	for name, v := range raw.Generations {
		var g string
		if json.Unmarshal(v, &g) == nil && g != "" {
			out.Generations[name] = g
		}
	}
	return out
}

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return io.ReadAll(io.LimitReader(f, limit))
}

func (s *OAuthStore) writeUnlocked(p *storeFile) error {
	p.Version = oauthStoreVersion
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	return fsutil.WriteFileAtomic(s.path, append(data, '\n'), 0o600)
}

// withLock runs f holding the store lock (auth/mcp.lock, as nanobot).
func (s *OAuthStore) withLock(f func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockFile(strings.TrimSuffix(s.path, filepath.Ext(s.path))+".lock", oauthStoreLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	return f()
}

// lockFile takes an exclusive lock on path, creating it, within timeout.
func lockFile(path string, timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := fsutil.LockExclusive(f, false)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, fsutil.ErrLocked) || time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", filepath.Base(path), err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// HasCredentials reports whether server name at serverURL (expanded) has
// an access token: whether a background connection can try it.
func (s *OAuthStore) HasCredentials(name, serverURL string) bool {
	snap, err := s.bind(name, serverURL).snapshot()
	return err == nil && snap.tokens != nil && snap.tokens.AccessToken != ""
}

// DeleteCredentials removes name's credentials, whatever its URL. It
// rotates the name's generation even without an entry, so a sign-in
// started before the removal cannot write them back. It reports whether
// there was an entry.
func (s *OAuthStore) DeleteCredentials(name string) (bool, error) {
	var removed bool
	err := s.withLock(func() error {
		p := s.readUnlocked()
		_, removed = p.Servers[name]
		delete(p.Servers, name)
		p.Generations[name] = newLease()
		return s.writeUnlocked(p)
	})
	return removed, err
}

// oauthBinding is one connection attempt's view of a server's entry. It
// observes the name's generation when created and, once it has written,
// holds the entry's write lease: a deletion (new generation) or a newer
// browser flow (new lease) makes its later writes no-ops.
type oauthBinding struct {
	store       *OAuthStore
	name        string
	fingerprint string
	generation  string

	mu    sync.Mutex
	lease string
}

func (s *OAuthStore) bind(name, serverURL string) *oauthBinding {
	// Writes replace the whole file, so an unlocked read sees either side
	// of a concurrent deletion.
	gen := s.readUnlocked().Generations[name]
	return &oauthBinding{store: s, name: name, fingerprint: serverFingerprint(serverURL), generation: gen}
}

func (b *oauthBinding) entryUnlocked(p *storeFile) *storedServer {
	e := p.Servers[b.name]
	if e == nil || e.ServerFingerprint != b.fingerprint {
		return nil
	}
	return e
}

// bindUnlocked returns the entry b may use, creating it if create is
// set, and whether the payload changed (a lease was assigned).
func (b *oauthBinding) bindUnlocked(p *storeFile, create bool) (*storedServer, bool) {
	if p.Generations[b.name] != b.generation {
		return nil, false
	}
	e := b.entryUnlocked(p)
	if b.lease != "" {
		if e == nil || e.WriteLease != b.lease {
			return nil, false
		}
		return e, false
	}
	if e == nil {
		if !create {
			return nil, false
		}
		b.lease = newLease()
		e = &storedServer{ServerFingerprint: b.fingerprint, WriteLease: b.lease}
		p.Servers[b.name] = e
		return e, true
	}
	changed := e.WriteLease == ""
	if changed {
		e.WriteLease = newLease()
	}
	b.lease = e.WriteLease
	return e, changed
}

// oauthSnapshot is a copy of an entry's state.
type oauthSnapshot struct {
	tokens      *storedTokens
	expiresAt   *float64
	tokenIssuer string
	client      *storedClient
	metadata    *storedMetadata
	oauthIssuer string
	redirectURI string
}

func snapshotOf(e *storedServer) oauthSnapshot {
	if e == nil {
		return oauthSnapshot{}
	}
	return oauthSnapshot{
		tokens: e.Tokens, expiresAt: e.ExpiresAt, tokenIssuer: e.TokenIssuer,
		client: e.ClientInfo, metadata: e.OAuthMetadata, oauthIssuer: e.OAuthIssuer, redirectURI: e.RedirectURI,
	}
}

func (b *oauthBinding) snapshot() (oauthSnapshot, error) {
	var snap oauthSnapshot
	err := b.store.withLock(func() error {
		b.mu.Lock()
		defer b.mu.Unlock()
		p := b.store.readUnlocked()
		e, changed := b.bindUnlocked(p, false)
		snap = snapshotOf(e)
		if changed {
			return b.store.writeUnlocked(p)
		}
		return nil
	})
	return snap, err
}

// update applies f to b's entry and saves it. With claim it takes the
// entry over with a new lease (a browser flow owns later writes); without
// it, the entry is created if create is set. It reports whether f ran:
// false when b is stale.
func (b *oauthBinding) update(f func(*storedServer), create, claim bool) (bool, error) {
	var ok bool
	err := b.store.withLock(func() error {
		b.mu.Lock()
		defer b.mu.Unlock()
		p := b.store.readUnlocked()
		var e *storedServer
		if claim {
			if p.Generations[b.name] != b.generation {
				b.store.logger.Info("ignored stale MCP OAuth credential claim", "server", b.name)
				return nil
			}
			e = b.entryUnlocked(p)
			if e == nil {
				e = &storedServer{ServerFingerprint: b.fingerprint}
				p.Servers[b.name] = e
			}
			b.lease = newLease()
			e.WriteLease = b.lease
		} else {
			hadLease := b.lease != ""
			e, _ = b.bindUnlocked(p, create)
			if e == nil {
				if hadLease {
					b.store.logger.Info("ignored stale MCP OAuth credential update", "server", b.name)
				}
				return nil
			}
		}
		f(e)
		ok = true
		return b.store.writeUnlocked(p)
	})
	return ok, err
}

// replaceTokens stores tok (nil clears the tokens).
func replaceTokens(e *storedServer, tok *oauth2.Token) {
	if tok == nil {
		e.Tokens, e.ExpiresAt, e.TokenIssuer = nil, nil, ""
		return
	}
	st := &storedTokens{AccessToken: tok.AccessToken, TokenType: tok.TokenType, RefreshToken: tok.RefreshToken}
	if sc, ok := tok.Extra("scope").(string); ok {
		st.Scope = sc
	}
	e.Tokens, e.ExpiresAt = st, nil
	if !tok.Expiry.IsZero() {
		at := float64(tok.Expiry.UnixMilli()) / 1000
		e.ExpiresAt = &at
		if in := time.Until(tok.Expiry); in > 0 {
			st.ExpiresIn = int64(in.Seconds())
		}
	}
}

// token converts stored tokens back.
func (snap oauthSnapshot) token() *oauth2.Token {
	if snap.tokens == nil || snap.tokens.AccessToken == "" {
		return nil
	}
	tok := &oauth2.Token{AccessToken: snap.tokens.AccessToken, TokenType: snap.tokens.TokenType, RefreshToken: snap.tokens.RefreshToken}
	if snap.expiresAt != nil {
		sec, frac := math.Modf(*snap.expiresAt)
		tok.Expiry = time.Unix(int64(sec), int64(frac*1e9))
	}
	if snap.tokens.Scope != "" {
		tok = tok.WithExtra(map[string]any{"scope": snap.tokens.Scope})
	}
	return tok
}

func (b *oauthBinding) clearTokens() (bool, error) {
	return b.update(func(e *storedServer) { replaceTokens(e, nil) }, false, false)
}

func (b *oauthBinding) clearMetadata() (bool, error) {
	return b.update(func(e *storedServer) { e.OAuthMetadata, e.OAuthIssuer = nil, "" }, false, false)
}

// saveAuthorization records a completed authorization: the server
// metadata and issuer, the client and the tokens, bound to that issuer.
func (b *oauthBinding) saveAuthorization(meta *storedMetadata, issuer string, client *storedClient, tok *oauth2.Token) (bool, error) {
	issuer = normalizeIssuer(issuer)
	return b.update(func(e *storedServer) {
		e.OAuthMetadata, e.OAuthIssuer = meta, issuer
		e.ClientInfo = client
		replaceTokens(e, tok)
		e.TokenIssuer = issuer
	}, true, false)
}

// prepareRedirectURI claims the entry for a browser flow. reset drops
// the tokens and client; a changed redirect URI drops the client, since
// dynamic registrations are bound to it. A stale claim (the server was
// removed since b was created) is ErrAuthorizationRequired.
func (b *oauthBinding) prepareRedirectURI(redirectURI string, reset bool) error {
	ok, err := b.update(func(e *storedServer) {
		changed := e.RedirectURI != redirectURI
		if reset {
			replaceTokens(e, nil)
			e.ClientInfo = nil
		} else if changed {
			e.ClientInfo = nil
		}
		e.RedirectURI = redirectURI
	}, true, true)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("MCP authorization was cancelled: %w", ErrAuthorizationRequired)
	}
	return nil
}

// refreshLease is the token binding a refresh started from; the refresh
// only commits if the entry still holds it.
type refreshLease struct {
	accessToken, refreshToken, tokenIssuer string
}

func leaseOf(e *storedServer) (refreshLease, bool) {
	if e == nil || e.Tokens == nil || e.Tokens.AccessToken == "" || e.Tokens.RefreshToken == "" || e.TokenIssuer == "" {
		return refreshLease{}, false
	}
	return refreshLease{e.Tokens.AccessToken, e.Tokens.RefreshToken, e.TokenIssuer}, true
}

// finishRefresh stores the refreshed tokens (nil clears them, and
// clearClient the client too) if the entry still holds lease. It reports
// whether it did.
func (b *oauthBinding) finishRefresh(lease refreshLease, tok *oauth2.Token, clearClient bool) (bool, error) {
	var ok bool
	err := b.store.withLock(func() error {
		b.mu.Lock()
		defer b.mu.Unlock()
		p := b.store.readUnlocked()
		e, changed := b.bindUnlocked(p, false)
		if cur, has := leaseOf(e); !has || cur != lease {
			if changed {
				return b.store.writeUnlocked(p)
			}
			return nil
		}
		issuer := e.TokenIssuer
		replaceTokens(e, tok)
		if tok != nil {
			e.TokenIssuer = issuer
		}
		if clearClient {
			e.ClientInfo = nil
		}
		ok = true
		return b.store.writeUnlocked(p)
	})
	return ok, err
}

// refreshLock serializes refreshes of one server across processes, so a
// rotated refresh token is used once.
func (b *oauthBinding) refreshLock() (func(), error) {
	sum := sha256.Sum256([]byte(b.name + "\x00" + b.fingerprint))
	path := filepath.Join(filepath.Dir(b.store.path), "mcp-refresh-"+hex.EncodeToString(sum[:])+".lock")
	return lockFile(path, oauthRefreshLockTimout)
}
