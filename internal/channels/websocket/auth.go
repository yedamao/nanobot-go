package websocket

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/yedamao/nanobot-go/internal/security"
)

const (
	// AuthPath reports whether this request must sign in (see AuthState).
	AuthPath = "/ui/auth"
	// LoginPath takes {"token":"..."}, the configured token or
	// tokenIssueSecret, and answers 204 with a session cookie.
	LoginPath = "/ui/login"
	// LogoutPath ends the request's session and clears its cookie.
	LogoutPath = "/ui/logout"
	// SessionCookie holds a random session id issued by LoginPath; the
	// secret itself is never stored in the browser.
	SessionCookie = "nanobot_go_session"
	// SessionTTL is how long a session lasts after sign-in.
	SessionTTL = 24 * time.Hour

	// maxSessions caps the sessions kept; the oldest is dropped first.
	maxSessions = 256
	// maxLoginBody caps the body of a sign-in request.
	maxLoginBody = 4 << 10
	// loginFailLimit failed sign-ins from one address within
	// loginFailWindow are answered 429 until the window passes.
	loginFailLimit  = 10
	loginFailWindow = 5 * time.Minute
	// maxFailAddrs caps the addresses whose failures are counted.
	maxFailAddrs = 1024
)

// AuthState is the body of GET /ui/auth.
type AuthState struct {
	// Required is whether this request must sign in to use the WebUI.
	Required bool `json:"required"`
	// Authenticated is whether it may use the WebUI now: it signed in, or
	// it needs no sign-in.
	Authenticated bool `json:"authenticated"`
}

// auth decides who may use the WebUI. A local request (loopback Host and
// loopback peer, not forwarded by a proxy) needs no credentials unless
// requireLocal is set; any other request needs a configured secret, sent
// as a Bearer or X-Nanobot-Auth header, or the session cookie LoginPath
// sets (or, on /ws only, ?token=). With no secret configured, only local
// requests are served.
type auth struct {
	// secrets are the SHA-256 digests of the configured token and
	// tokenIssueSecret; compared in constant time.
	secrets      [][sha256.Size]byte
	requireLocal bool
	now          func() time.Time

	mu sync.Mutex
	// sessions maps a session id's digest to its expiry.
	sessions map[[sha256.Size]byte]time.Time
	// failures counts recent failed sign-ins by peer address.
	failures map[string]failCount
}

type failCount struct {
	n     int
	since time.Time
}

func newAuth(cfg Config) *auth {
	a := &auth{
		requireLocal: cfg.WebsocketRequiresToken != nil && *cfg.WebsocketRequiresToken,
		now:          time.Now,
		sessions:     map[[sha256.Size]byte]time.Time{},
		failures:     map[string]failCount{},
	}
	for _, s := range []string{cfg.Token, cfg.TokenIssueSecret} {
		if s != "" {
			a.secrets = append(a.secrets, sha256.Sum256([]byte(s)))
		}
	}
	return a
}

// enabled reports whether a secret is configured, so remote requests can
// sign in.
func (a *auth) enabled() bool { return len(a.secrets) > 0 }

// isSecret reports whether s is a configured secret. Every secret is
// compared, so the time taken does not tell which one matched.
func (a *auth) isSecret(s string) bool {
	if s == "" {
		return false
	}
	got := sha256.Sum256([]byte(s))
	ok := 0
	for _, want := range a.secrets {
		ok |= subtle.ConstantTimeCompare(got[:], want[:])
	}
	return ok == 1
}

// isSession reports whether id is a live session.
func (a *auth) isSession(id string) bool {
	if id == "" {
		return false
	}
	key := sha256.Sum256([]byte(id))
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[key]
	if ok && !a.now().Before(exp) {
		delete(a.sessions, key)
		return false
	}
	return ok
}

// newSession issues a session id valid for SessionTTL.
func (a *auth) newSession() string {
	id := rand.Text()
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	var oldest [sha256.Size]byte
	var oldestExp time.Time
	for k, exp := range a.sessions {
		if !now.Before(exp) {
			delete(a.sessions, k)
		} else if oldestExp.IsZero() || exp.Before(oldestExp) {
			oldest, oldestExp = k, exp
		}
	}
	if len(a.sessions) >= maxSessions {
		delete(a.sessions, oldest)
	}
	a.sessions[sha256.Sum256([]byte(id))] = now.Add(SessionTTL)
	return id
}

func (a *auth) endSession(id string) {
	key := sha256.Sum256([]byte(id))
	a.mu.Lock()
	delete(a.sessions, key)
	a.mu.Unlock()
}

// isLocal reports whether r comes from this device to a loopback name:
// its Host and its peer are loopback and it carries no proxy forwarding
// header. A LAN client sending Host: 127.0.0.1 is not local, and neither
// is a request a local reverse proxy forwards with such a header.
func isLocal(r *http.Request) bool {
	if !security.IsLoopbackHost(requestHost(r)) {
		return false
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(peer)
	if err != nil || !ip.Unmap().IsLoopback() {
		return false
	}
	for _, h := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

func requestHost(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		return h
	}
	return r.Host
}

// authenticated reports whether r presents a configured secret (as a
// Bearer or X-Nanobot-Auth header) or a live session cookie; with
// allowQuery, ?token= may carry either.
func (a *auth) authenticated(r *http.Request, allowQuery bool) bool {
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && a.isSecret(strings.TrimSpace(token)) {
		return true
	}
	if a.isSecret(strings.TrimSpace(r.Header.Get("X-Nanobot-Auth"))) {
		return true
	}
	if c, err := r.Cookie(SessionCookie); err == nil && a.isSession(c.Value) {
		return true
	}
	if allowQuery {
		if t := r.URL.Query().Get("token"); a.isSecret(t) || a.isSession(t) {
			return true
		}
	}
	return false
}

// state returns r's AuthState. ok is false when r cannot use the WebUI at
// all: it is not local and no secret is configured.
func (a *auth) state(r *http.Request, allowQuery bool) (st AuthState, ok bool) {
	if isLocal(r) && !a.requireLocal {
		return AuthState{Authenticated: true}, true
	}
	if !a.enabled() {
		return AuthState{}, false
	}
	return AuthState{Required: true, Authenticated: a.authenticated(r, allowQuery)}, true
}

// protect serves next only to requests that may use the WebUI: others get
// 403 when they can never sign in (no secret configured, so only
// loopback is served) and 401 when they have not signed in.
func (a *auth) protect(next http.Handler) http.Handler {
	return a.guard(next, false)
}

// protectWS is protect for /ws, which also takes ?token= since browsers
// cannot set headers on a WebSocket.
func (a *auth) protectWS(next http.Handler) http.Handler {
	return a.guard(next, true)
}

func (a *auth) guard(next http.Handler, allowQuery bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st, ok := a.state(r, allowQuery)
		switch {
		case !ok:
			http.Error(w, "the WebUI answers only on loopback addresses", http.StatusForbidden)
		case !st.Authenticated:
			unauthorized(w)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// public serves next to anyone who could sign in: the page and its
// assets, so a remote browser can load the sign-in form. With no secret
// configured it is loopback only, as before.
func (a *auth) public(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.state(r, false); !ok {
			http.Error(w, "the WebUI answers only on loopback addresses", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="nanobot-go"`)
	http.Error(w, "sign in required", http.StatusUnauthorized)
}

// serveAuth answers GET /ui/auth with the request's AuthState.
func (a *auth) serveAuth(w http.ResponseWriter, r *http.Request) {
	st, _ := a.state(r, false)
	writeJSON(w, st)
}

// LoginRequest is the body of POST /ui/login.
type LoginRequest struct {
	Token string `json:"token"`
}

// login answers POST /ui/login: a configured secret gets 204 and a
// session cookie, anything else 401 (or 429 after too many failures
// from the same address). It never says which secret matched.
func (a *auth) login(w http.ResponseWriter, r *http.Request) {
	if !a.enabled() {
		http.Error(w, "sign-in is not configured: set channels.websocket.tokenIssueSecret", http.StatusNotFound)
		return
	}
	peer := peerAddr(r)
	if a.blocked(peer) {
		http.Error(w, "too many failed sign-ins; try again later", http.StatusTooManyRequests)
		return
	}
	var req LoginRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLoginBody))
	if err := dec.Decode(&req); err != nil {
		http.Error(w, `body must be {"token":"..."}`, http.StatusBadRequest)
		return
	}
	if !a.isSecret(strings.TrimSpace(req.Token)) {
		a.failed(peer)
		unauthorized(w)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    a.newSession(),
		Path:     "/",
		MaxAge:   int(SessionTTL / time.Second),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// logout answers POST /ui/logout: it ends the cookie's session, if any,
// and clears the cookie.
func (a *auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		a.endSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func peerAddr(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func (a *auth) blocked(peer string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.failures[peer]
	if ok && a.now().Sub(f.since) >= loginFailWindow {
		delete(a.failures, peer)
		return false
	}
	return f.n >= loginFailLimit
}

func (a *auth) failed(peer string) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.failures[peer]
	if !ok || now.Sub(f.since) >= loginFailWindow {
		if !ok && len(a.failures) >= maxFailAddrs {
			for k, v := range a.failures {
				if now.Sub(v.since) >= loginFailWindow {
					delete(a.failures, k)
				}
			}
			if len(a.failures) >= maxFailAddrs {
				return
			}
		}
		f = failCount{since: now}
	}
	f.n++
	a.failures[peer] = f
}
