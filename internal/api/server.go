// Package api serves an OpenAI-compatible HTTP API in front of the agent
// loop, like nanobot/api/server.py:
//
//	GET  /health               {"status":"ok"}, never authenticated
//	GET  /v1/models            the configured model
//	POST /v1/chat/completions  one agent turn (tools included)
//
// Each request is one turn in a server-side session ("api:<session_id>",
// or "api:default"): the server keeps the history, so a request must carry
// exactly one user message. Turns of one session run in order.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yedamao/nanobot-go/internal/agent"
	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/security"
)

const (
	// Channel is the synthetic channel API turns run in.
	Channel = "api"
	// DefaultSessionKey is the session of requests without a session_id.
	DefaultSessionKey = Channel + ":default"
	// chatID is the chat every API turn runs in, like nanobot's
	// API_CHAT_ID; the session key tells conversations apart.
	chatID = "default"
	// senderID matches what nanobot's process_direct uses.
	senderID = "user"

	// MaxBodyBytes caps a request body. nanobot allows 20 MiB for base64
	// images; text-only requests need far less.
	MaxBodyBytes = 4 << 20
	// maxSessionID caps session_id, which becomes part of a session key.
	maxSessionID = 256
	// ownedBy is /v1/models' owned_by.
	ownedBy = "nanobot"

	// keepAliveInterval is how often a stream sends an SSE comment while
	// the turn runs, so proxies and clients do not give up on it.
	keepAliveInterval = 15 * time.Second

	readHeaderTimeout = 10 * time.Second
	// readTimeout bounds reading a request; the server clears the read
	// deadline before the handler runs, so turns are not cut short.
	readTimeout    = 30 * time.Second
	idleTimeout    = 60 * time.Second
	maxHeaderBytes = 64 << 10
)

// TurnFunc runs one agent turn; agent.Loop.Process is one.
type TurnFunc func(ctx context.Context, msg bus.InboundMessage) (agent.Reply, error)

// Config configures a Server.
type Config struct {
	// Host and Port are where Start listens. A host beyond loopback
	// requires APIKey.
	Host string
	Port int
	// APIKey, when set, is the bearer token every route but /health
	// requires.
	APIKey string
	// Model is the one model /v1/models lists and requests may name.
	Model string
	// Timeout bounds each request's turn, including any wait behind
	// earlier turns of the same session.
	Timeout time.Duration
	// Turn runs the turns.
	Turn TurnFunc
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Server is the API. It is an http.Handler; Start and Shutdown run it on
// its own listener.
type Server struct {
	cfg       Config
	keyDigest [sha256.Size]byte
	log       *slog.Logger
	handler   http.Handler
	keepAlive time.Duration

	mu   sync.Mutex
	srv  *http.Server
	addr string
}

// New returns a Server. Like `nanobot serve`, it refuses a host beyond
// loopback without an API key, so the agent is never exposed to the
// network unauthenticated.
func New(cfg Config) (*Server, error) {
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	switch {
	case cfg.Turn == nil:
		return nil, errors.New("api: turn function is required")
	case strings.TrimSpace(cfg.Model) == "":
		return nil, errors.New("api: model is required")
	case cfg.Timeout <= 0:
		return nil, fmt.Errorf("api: timeout must be positive, got %v", cfg.Timeout)
	case !security.IsLoopbackHost(cfg.Host) && cfg.APIKey == "":
		return nil, fmt.Errorf("api: host %q is reachable beyond this device but api.apiKey is not set; "+
			"set api.apiKey (or NANOBOT_GO_API_KEY) to prevent unauthenticated access", cfg.Host)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{cfg: cfg, log: log, keepAlive: keepAliveInterval}
	if cfg.APIKey != "" {
		s.keyDigest = sha256.Sum256([]byte(cfg.APIKey))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "Unknown route "+r.URL.Path, errInvalidRequest)
	})
	s.handler = s.logRequests(s.authenticate(mux))
	return s, nil
}

// ServeHTTP serves the API's routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Start listens on Host:Port and serves in the background. Request
// contexts derive from base: canceling it cancels the turns in flight.
// It returns the address it listens on.
func (s *Server) Start(base context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return "", errors.New("api: already started")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port)))
	if err != nil {
		return "", fmt.Errorf("api: %w", err)
	}
	s.srv = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		// No WriteTimeout: a turn (and its stream) may take minutes; the
		// turn timeout bounds it instead.
		IdleTimeout:    idleTimeout,
		MaxHeaderBytes: maxHeaderBytes,
		BaseContext:    func(net.Listener) context.Context { return base },
		ErrorLog:       slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	s.addr = ln.Addr().String()
	go func(srv *http.Server) {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("api server stopped", "error", err)
		}
	}(s.srv)
	return s.addr, nil
}

// Shutdown stops accepting requests and waits for the ones in flight
// until ctx ends, then closes their connections. Cancel Start's base
// context to cut the turns themselves short.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	if err := srv.Shutdown(ctx); err != nil {
		srv.Close()
		return fmt.Errorf("api: shutdown: %w", err)
	}
	return nil
}

// Error types of OpenAI-style error bodies.
const (
	errInvalidRequest = "invalid_request_error"
	errServer         = "server_error"
)

// writeError writes nanobot's {"error":{"message","type","code"}} body.
func writeError(w http.ResponseWriter, status int, message, errType string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": errType, "code": status}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// allowMethod writes a 405 unless r uses one of methods.
func allowMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "Method "+r.Method+" not allowed", errInvalidRequest)
	return false
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if allowMethod(w, r, http.MethodGet, http.MethodHead) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   []any{map[string]any{"id": s.cfg.Model, "object": "model", "created": 0, "owned_by": ownedBy}},
	})
}

// authenticate requires the bearer token on every route but /health when
// an API key is configured. Digests are compared so neither the key's
// content nor its length leaks through timing.
func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.cfg.APIKey == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "Missing Authorization header. Use: Bearer <api_key>", errInvalidRequest)
			return
		}
		got := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(got[:], s.keyDigest[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "Invalid API key", errInvalidRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests tags each response with an X-Request-ID and logs it, like
// nanobot's request logging middleware. Request bodies are not logged.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := randomHex(8)
		w.Header().Set("X-Request-ID", id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		level := slog.LevelInfo
		if r.URL.Path == "/health" {
			level = slog.LevelDebug
		}
		s.log.Log(r.Context(), level, "api request", "request_id", id, "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

// statusRecorder remembers the response status. Unwrap lets
// http.ResponseController reach the connection's Flush.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(p)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// randomHex returns n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails
	return hex.EncodeToString(b)
}
