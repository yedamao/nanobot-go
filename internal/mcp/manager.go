// Package mcp connects to Model Context Protocol servers and exposes their
// tools to the agent as mcp_<server>_<tool>. It ports
// nanobot/agent/tools/mcp.py on top of the official Go SDK.
//
// The application owns a Manager, not the agent loop: it creates one with
// the shared tool registry, calls Connect before the first turn and Close
// at shutdown; Reload applies a changed server set in between. Tools are
// registered and unregistered while the agent may be running; the
// registry is safe for that.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// DefaultConnectTimeout bounds connecting to one server and listing its
// tools.
const DefaultConnectTimeout = 30 * time.Second

// reconnectBackoff stops every call to a dead server from paying for a
// fresh connection attempt.
const reconnectBackoff = 30 * time.Second

// stderrTail is how much of a stdio server's stderr goes into its error.
const stderrTail = 2048

// closeWait bounds how long Close waits for background goroutines after
// closing every session.
const closeWait = 10 * time.Second

// State is a server's connection state.
type State string

// Server states.
const (
	StatePending   State = "pending" // Connect not called yet
	StateConnected State = "connected"
	StateFailed    State = "failed"
	StateDisabled  State = "disabled" // enabled: false in the config
	StateClosed    State = "closed"
	// StateAuthorizationRequired: an OAuth server without usable
	// credentials, waiting for a browser sign-in from the WebUI.
	StateAuthorizationRequired State = "authorization_required"
)

// ServerStatus describes one configured server.
type ServerStatus struct {
	Name      string
	Transport string
	State     State
	// Tools are the registered model-facing tool names, sorted.
	Tools []string
	// Err is the last connection error, if any.
	Err error
}

// errClosed is returned once the Manager is closed.
var errClosed = errors.New("MCP manager is closed")

// errRetired is returned for a server Reload removed or replaced.
var errRetired = errors.New("MCP server was reconfigured")

// Option configures a Manager.
type Option func(*Manager)

// WithTransport replaces the default transports (NewTransportFactory),
// e.g. with in-memory transports in tests.
func WithTransport(f TransportFactory) Option { return func(m *Manager) { m.newTransport = f } }

// WithSSRFPolicy sets which addresses HTTP servers may be reached at
// (tools.ssrfWhitelist); the default policy blocks loopback and private
// ranges. It is ignored with WithTransport.
func WithSSRFPolicy(p *security.Policy) Option { return func(m *Manager) { m.ssrf = p } }

// WithConnectTimeout sets the per-server connect timeout.
func WithConnectTimeout(d time.Duration) Option { return func(m *Manager) { m.connectTimeout = d } }

// WithStderr copies stdio servers' stderr to w (by default it is only kept
// for error messages).
func WithStderr(w io.Writer) Option { return func(m *Manager) { m.stderr = w } }

// WithVersion sets the client version reported to servers.
func WithVersion(v string) Option { return func(m *Manager) { m.version = v } }

// WithLookupEnv sets where ${VAR} references are looked up (default
// os.LookupEnv).
func WithLookupEnv(f func(string) (string, bool)) Option {
	return func(m *Manager) { m.lookupEnv = f }
}

// WithOAuthStore sets where OAuth servers' credentials are kept
// (config.MCPOAuthPath). Without it, OAuth servers stay
// StateAuthorizationRequired.
func WithOAuthStore(st *OAuthStore) Option { return func(m *Manager) { m.oauth = st } }

// WithOAuthBrowser lets server name authorize in the browser through b
// when it has no valid credentials. Only a user-started attempt (the
// WebUI's sign-in) passes it; background connections never prompt.
func WithOAuthBrowser(name string, b *OAuthBrowser) Option {
	return func(m *Manager) {
		if m.browsers == nil {
			m.browsers = map[string]*OAuthBrowser{}
		}
		m.browsers[name] = b
	}
}

// Manager owns the connections to the configured MCP servers and their
// tool registrations. It is safe for concurrent use.
type Manager struct {
	reg            *tools.Registry
	logger         *slog.Logger
	newTransport   TransportFactory
	ssrf           *security.Policy
	connectTimeout time.Duration
	stderr         io.Writer
	version        string
	lookupEnv      func(string) (string, bool)
	oauth          *OAuthStore
	browsers       map[string]*OAuthBrowser
	client         *sdk.Client

	// mu guards servers, which is sorted by name. Reload replaces the
	// slice; it is never modified in place.
	mu      sync.Mutex
	servers []*server
	// reloadMu serializes Reload.
	reloadMu sync.Mutex

	// ctx outlives every session (SSE streams are bound to the context
	// they are opened with); Close cancels it.
	ctx    context.Context
	cancel context.CancelFunc

	// goMu orders spawn against Close so wg.Add never races wg.Wait.
	goMu      sync.Mutex
	closed    atomic.Bool
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// server is one configured server. mu guards the fields below it;
// connectMu serializes connection attempts and is taken before mu.
type server struct {
	name string
	cfg  config.MCPServerConfig

	connectMu sync.Mutex

	mu         sync.Mutex
	state      State
	err        error
	conn       *conn
	registered bool     // tools were registered (on the first connect)
	tools      []string // registered names, sorted
	failedAt   time.Time
	retired    bool // removed or replaced by Reload: never connects again
}

// conn is one live session. Calls are bound to ctx, so close can cancel
// them: the SDK's Close otherwise waits for in-flight calls to return.
type conn struct {
	session *sdk.ClientSession
	ctx     context.Context // the session's lifetime
	cancel  context.CancelFunc
	secrets []string // redacted from call errors
}

func (c *conn) close() {
	c.cancel()
	_ = c.session.Close()
}

// NewManager returns a Manager for servers. Nothing connects until
// Connect. reg receives the tools; logger may be nil.
func NewManager(servers map[string]config.MCPServerConfig, reg *tools.Registry, logger *slog.Logger, opts ...Option) *Manager {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	m := &Manager{
		reg:            reg,
		logger:         logger,
		connectTimeout: DefaultConnectTimeout,
		version:        "dev",
		lookupEnv:      os.LookupEnv,
	}
	for _, o := range opts {
		o(m)
	}
	if m.newTransport == nil {
		m.newTransport = NewTransportFactory(m.ssrf)
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	// Advertise no client capabilities: nanobot-go serves no roots,
	// sampling or elicitation.
	m.client = sdk.NewClient(&sdk.Implementation{Name: "nanobot-go", Version: m.version},
		&sdk.ClientOptions{Logger: logger, Capabilities: &sdk.ClientCapabilities{}})
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		m.servers = append(m.servers, newServer(name, servers[name]))
	}
	return m
}

func newServer(name string, cfg config.MCPServerConfig) *server {
	s := &server{name: name, cfg: cfg, state: StatePending}
	if !cfg.IsEnabled() {
		s.state = StateDisabled
	}
	return s
}

// snapshot returns the current servers.
func (m *Manager) snapshot() []*server {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.servers
}

// Connect connects every enabled server that is not connected yet,
// concurrently, and registers their tools. A server that fails is logged
// and recorded in Status; the others are unaffected. Connect returns an
// error only if ctx ends first.
func (m *Manager) Connect(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, s := range m.snapshot() {
		if s.cfg.IsEnabled() {
			wg.Go(func() { _, _ = m.connect(ctx, s, nil) })
		}
	}
	wg.Wait()
	return ctx.Err()
}

// connect (re)connects s unless it is already connected through a
// connection other than stale. The first successful connection registers
// the server's tools; later ones reuse them, since each tool looks up the
// server's current session when called.
func (m *Manager) connect(ctx context.Context, s *server, stale *conn) (*conn, error) {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	if m.closed.Load() {
		return nil, errClosed
	}
	s.mu.Lock()
	if s.retired {
		s.mu.Unlock()
		return nil, errRetired
	}
	cur := s.conn
	if cur != nil && cur != stale {
		s.mu.Unlock()
		return cur, nil // connected meanwhile
	}
	s.conn = nil
	s.mu.Unlock()
	if cur != nil {
		cur.close()
	}

	c, defs, err := m.dial(ctx, s)

	s.mu.Lock()
	if m.closed.Load() {
		// Close may already have passed this server; clean up here.
		s.mu.Unlock()
		if c != nil {
			c.close()
		}
		return nil, errClosed
	}
	defer s.mu.Unlock()
	if err != nil {
		s.state, s.err, s.failedAt = StateFailed, err, time.Now()
		if errors.Is(err, ErrAuthorizationRequired) {
			s.state = StateAuthorizationRequired
			m.logger.Info("MCP server is waiting for browser authorization", "server", s.name)
			return nil, err
		}
		m.logger.Info("MCP server failed to connect", "server", s.name, "error", err)
		return nil, err
	}
	if !s.registered {
		s.tools = m.register(s, defs)
		s.registered = true
	}
	s.conn, s.state, s.err, s.failedAt = c, StateConnected, nil, time.Time{}
	// If Close starts now, spawn fails, and Close (waiting for s.mu)
	// will find and close c.
	if !m.spawn(func() { m.watch(s, c) }) {
		return nil, errClosed
	}
	m.logger.Info("MCP server connected", "server", s.name, "tools", len(s.tools))
	return c, nil
}

// watch marks s failed when its session ends on its own (the server
// exited or the connection broke).
func (m *Manager) watch(s *server, c *conn) {
	_ = c.session.Wait()
	s.mu.Lock()
	lost := s.conn == c
	if lost {
		s.conn, s.state, s.err = nil, StateFailed, errors.New("connection lost")
	}
	s.mu.Unlock()
	if lost {
		m.logger.Warn("MCP server connection lost", "server", s.name)
	}
	c.close() // reap a stdio child and release the lifetime context
}

// dial opens a session to s and lists its tools within the connect
// timeout.
func (m *Manager) dial(ctx context.Context, s *server) (*conn, []*sdk.Tool, error) {
	cfg, err := s.cfg.Expand(m.lookupEnv)
	if err != nil {
		return nil, nil, err
	}
	secrets := secretsOf(cfg)
	c, defs, err := m.open(ctx, s, cfg)
	if err != nil {
		return nil, nil, redact(err, secrets)
	}
	c.secrets = secrets
	return c, defs, nil
}

// open does the work of dial with the expanded config.
func (m *Manager) open(ctx context.Context, s *server, cfg config.MCPServerConfig) (*conn, []*sdk.Tool, error) {
	tail := &tailBuffer{max: stderrTail}
	var stderr io.Writer = tail
	if m.stderr != nil {
		stderr = io.MultiWriter(tail, m.stderr)
	}
	var oa *oauthAuth
	if cfg.UsesOAuth() {
		if t := cfg.Transport(); t != config.MCPStreamableHTTP && t != config.MCPSSE {
			return nil, nil, fmt.Errorf("OAuth needs a %s or %s server", config.MCPStreamableHTTP, config.MCPSSE)
		}
		var err error
		if oa, err = m.newOAuth(s.name, cfg); err != nil {
			return nil, nil, err
		}
	}
	transport, err := m.newTransport(s.name, cfg, stderr)
	if err != nil {
		return nil, nil, err
	}
	if oa != nil {
		wrapOAuth(transport, oa)
	}
	deadline := time.Now().Add(m.connectTimeout)

	// The session's lifetime context must outlive the connect timeout,
	// so the timeout is applied by waiting rather than by canceling it.
	lifeCtx, cancel := context.WithCancel(m.ctx)
	type result struct {
		cs  *sdk.ClientSession
		err error
	}
	done := make(chan result) // unbuffered: an abandoned result goes to the abandon branch
	abandon := make(chan struct{})
	started := m.spawn(func() {
		cs, err := m.client.Connect(lifeCtx, transport, nil)
		select {
		case done <- result{cs, err}:
		case <-abandon:
			if cs != nil {
				_ = cs.Close()
			}
		}
	})
	if !started {
		cancel()
		return nil, nil, errClosed
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var r result
	select {
	case r = <-done:
	case <-timer.C:
		r.err = fmt.Errorf("timed out after %s", m.connectTimeout)
	case <-ctx.Done():
		r.err = ctx.Err()
	}
	if r.cs == nil {
		close(abandon)
		cancel()
		return nil, nil, withStderr(fmt.Errorf("connect: %w", r.err), tail)
	}
	c := &conn{session: r.cs, ctx: lifeCtx, cancel: cancel}

	lctx, lcancel := context.WithDeadline(ctx, deadline)
	defer lcancel()
	defs, err := listTools(lctx, r.cs)
	if err != nil {
		c.close()
		return nil, nil, withStderr(fmt.Errorf("list tools: %w", err), tail)
	}
	return c, defs, nil
}

// secretError hides configured secrets in an error's text while keeping
// it unwrappable.
type secretError struct {
	msg string
	err error
}

func (e *secretError) Error() string { return e.msg }
func (e *secretError) Unwrap() error { return e.err }

// secretsOf returns the values that must not show up in errors or logs:
// env and header values (and their last word, for "Bearer <token>"), and
// the URL's query and password.
func secretsOf(cfg config.MCPServerConfig) []string {
	var out []string
	add := func(s string) {
		if len(s) >= 4 { // shorter values would mangle unrelated text
			out = append(out, s)
		}
	}
	for _, m := range []map[string]string{cfg.Env, cfg.Headers} {
		for _, v := range m {
			add(v)
			if f := strings.Fields(v); len(f) > 1 {
				add(f[len(f)-1])
			}
		}
	}
	if u, err := url.Parse(cfg.URL); err == nil {
		add(u.RawQuery)
		if p, ok := u.User.Password(); ok {
			add(p)
		}
	}
	// Longest first, so a value containing another is replaced whole.
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// redact replaces secrets in err's text with "[redacted]".
func redact(err error, secrets []string) error {
	if err == nil || len(secrets) == 0 {
		return err
	}
	msg := err.Error()
	for _, s := range secrets {
		msg = strings.ReplaceAll(msg, s, "[redacted]")
	}
	if msg == err.Error() {
		return err
	}
	return &secretError{msg: msg, err: err}
}

func withStderr(err error, tail *tailBuffer) error {
	if s := tail.String(); s != "" {
		return fmt.Errorf("%w (stderr: %s)", err, s)
	}
	return err
}

// listTools pages through tools/list.
func listTools(ctx context.Context, cs *sdk.ClientSession) ([]*sdk.Tool, error) {
	if init := cs.InitializeResult(); init != nil && init.Capabilities != nil && init.Capabilities.Tools == nil {
		return nil, nil // the server offers no tools
	}
	var defs []*sdk.Tool
	seen := map[string]bool{}
	params := &sdk.ListToolsParams{}
	for {
		res, err := cs.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		defs = append(defs, res.Tools...)
		if res.NextCursor == "" {
			return defs, nil
		}
		if seen[res.NextCursor] {
			return nil, errors.New("tools/list repeated a pagination cursor")
		}
		seen[res.NextCursor] = true
		params = &sdk.ListToolsParams{Cursor: res.NextCursor}
	}
}

// register wraps the enabled tools of s and adds them to the registry,
// skipping (and logging) name collisions. It returns the registered names.
func (m *Manager) register(s *server, defs []*sdk.Tool) []string {
	allowed := s.cfg.AllowedTools()
	all := slices.Contains(allowed, config.MCPAllTools)
	unmatched := map[string]bool{}
	for _, a := range allowed {
		unmatched[a] = a != config.MCPAllTools
	}
	var wrapped []tools.Tool
	byName := map[string]string{} // model-facing name -> remote name
	for _, d := range defs {
		if d == nil {
			continue
		}
		name := toolName(s.name, d.Name)
		if !all {
			_, rawOK := unmatched[d.Name]
			_, nameOK := unmatched[name]
			if !rawOK && !nameOK {
				continue
			}
			delete(unmatched, d.Name)
			delete(unmatched, name)
		}
		if prev, dup := byName[name]; dup {
			m.logger.Warn("MCP tool name collision; skipping tool", "server", s.name, "tool", d.Name, "name", name, "kept", prev)
			continue
		}
		byName[name] = d.Name
		wrapped = append(wrapped, m.newTool(s, name, d))
	}
	for a, missing := range unmatched {
		if missing {
			m.logger.Warn("MCP enabledTools entry matches no tool", "server", s.name, "entry", a)
		}
	}
	var names []string
	for i, err := range m.reg.RegisterEach(wrapped...) {
		if err != nil {
			m.logger.Warn("MCP tool not registered", "server", s.name, "tool", wrapped[i].(*remoteTool).remote, "error", err)
			continue
		}
		names = append(names, wrapped[i].Name())
	}
	slices.Sort(names)
	return names
}

// session returns the server's live connection, reconnecting once if it
// has none (subject to reconnectBackoff).
func (m *Manager) session(ctx context.Context, s *server) (*conn, error) {
	if m.closed.Load() {
		return nil, errClosed
	}
	s.mu.Lock()
	c, err, failedAt := s.conn, s.err, s.failedAt
	s.mu.Unlock()
	if c != nil {
		return c, nil
	}
	if !failedAt.IsZero() && time.Since(failedAt) < reconnectBackoff {
		return nil, err
	}
	return m.connect(ctx, s, nil)
}

// Reconnect connects server name now if it is not connected, ignoring
// the reconnect backoff: after a browser sign-in stored its credentials.
// A disabled or unknown server is an error.
func (m *Manager) Reconnect(ctx context.Context, name string) error {
	for _, s := range m.snapshot() {
		if s.name != name {
			continue
		}
		if !s.cfg.IsEnabled() {
			return fmt.Errorf("MCP server %s is disabled", name)
		}
		_, err := m.connect(ctx, s, nil)
		return err
	}
	return fmt.Errorf("no MCP server %s", name)
}

// spawn runs f in a goroutine that Close waits for. It reports false,
// without running f, once Close has started.
func (m *Manager) spawn(f func()) bool {
	m.goMu.Lock()
	defer m.goMu.Unlock()
	if m.closed.Load() {
		return false
	}
	m.wg.Go(f)
	return true
}

// Status reports every configured server, sorted by name.
func (m *Manager) Status() []ServerStatus {
	servers := m.snapshot()
	out := make([]ServerStatus, 0, len(servers))
	for _, s := range servers {
		s.mu.Lock()
		out = append(out, ServerStatus{
			Name:      s.name,
			Transport: s.cfg.Transport(),
			State:     s.state,
			Tools:     slices.Clone(s.tools),
			Err:       s.err,
		})
		s.mu.Unlock()
	}
	return out
}

// Close unregisters every MCP tool and ends every session, terminating
// stdio servers. It waits for background work to stop. Close is
// idempotent and safe to call concurrently; later calls wait for the
// first to finish.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.goMu.Lock()
		m.mu.Lock() // a Reload after this sees closed and adds no servers
		m.closed.Store(true)
		m.mu.Unlock()
		m.goMu.Unlock()

		var conns []*conn
		for _, s := range m.snapshot() {
			s.mu.Lock()
			if s.conn != nil {
				conns = append(conns, s.conn)
			}
			m.reg.Unregister(s.tools...)
			s.conn, s.tools = nil, nil
			if s.state != StateDisabled {
				s.state = StateClosed
			}
			s.mu.Unlock()
		}
		var wg sync.WaitGroup
		for _, c := range conns {
			wg.Go(c.close)
		}
		wg.Wait()
		m.cancel() // aborts connection attempts still in flight
		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(closeWait):
			// Only a transport stuck in a blocking write gets here; don't
			// hang the process on it.
			m.logger.Warn("MCP background work did not stop in time", "waited", closeWait)
		}
	})
	return nil
}
