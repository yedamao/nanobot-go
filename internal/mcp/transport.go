package mcp

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/security"
)

// TransportFactory builds the transport for one connection attempt. It is
// called again for every reconnect. cfg has ${VAR} references expanded.
// stderr receives a stdio server's stderr.
type TransportFactory func(name string, cfg config.MCPServerConfig, stderr io.Writer) (sdk.Transport, error)

// stdioTerminateWait is how long closing a stdio server waits for it to
// exit after its stdin is closed, and again after SIGTERM, before killing
// it.
const stdioTerminateWait = 2 * time.Second

// stdioEnvKeys are inherited by stdio servers, like the Python MCP SDK's
// get_default_environment. Everything else, notably provider API keys,
// is withheld unless set in the server's env.
var stdioEnvKeys = []string{
	"HOME", "LOGNAME", "PATH", "SHELL", "TERM", "USER", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE",
	// Windows essentials.
	"APPDATA", "HOMEDRIVE", "HOMEPATH", "LOCALAPPDATA", "PATHEXT", "PROCESSOR_ARCHITECTURE",
	"SYSTEMDRIVE", "SYSTEMROOT", "TEMP", "TMP", "USERNAME", "USERPROFILE", "COMSPEC",
}

// DefaultTransport builds SDK transports from the config with the default
// SSRF policy; see NewTransportFactory.
func DefaultTransport(name string, cfg config.MCPServerConfig, stderr io.Writer) (sdk.Transport, error) {
	return newTransport(cfg, stderr, nil)
}

// NewTransportFactory returns a factory that builds a child process for
// stdio servers and, for HTTP servers, clients that add the configured
// headers and only reach addresses policy allows (nil: the default
// policy, which blocks loopback and private ranges).
func NewTransportFactory(policy *security.Policy) TransportFactory {
	return func(_ string, cfg config.MCPServerConfig, stderr io.Writer) (sdk.Transport, error) {
		return newTransport(cfg, stderr, policy)
	}
}

func newTransport(cfg config.MCPServerConfig, stderr io.Writer, policy *security.Policy) (sdk.Transport, error) {
	switch t := cfg.Transport(); t {
	case config.MCPStdio:
		cmd := exec.Command(cfg.Command, cfg.Args...)
		cmd.Env = stdioEnv(cfg.Env)
		if cfg.Cwd != "" {
			dir, err := config.ExpandHome(cfg.Cwd)
			if err != nil {
				return nil, err
			}
			cmd.Dir = dir
		}
		cmd.Stderr = stderr
		return &sdk.CommandTransport{Command: cmd, TerminateDuration: stdioTerminateWait}, nil
	case config.MCPStreamableHTTP:
		return &sdk.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: httpClient(httpHeaders(cfg), policy)}, nil
	case config.MCPSSE:
		return &sdk.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: httpClient(httpHeaders(cfg), policy)}, nil
	default:
		return nil, fmt.Errorf("unknown MCP transport %q", t)
	}
}

// httpHeaders are the headers to send: with OAuth, all but
// Authorization, which carries the OAuth token instead.
func httpHeaders(cfg config.MCPServerConfig) map[string]string {
	if !cfg.UsesOAuth() {
		return cfg.Headers
	}
	out := map[string]string{}
	for k, v := range cfg.Headers {
		if !strings.EqualFold(k, "Authorization") {
			out[k] = v
		}
	}
	return out
}

func stdioEnv(extra map[string]string) []string {
	var env []string
	for _, k := range stdioEnvKeys {
		if _, set := extra[k]; set {
			continue
		}
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		env = append(env, k+"="+extra[k])
	}
	return env
}

// maxHTTPRedirects matches net/http's default redirect limit.
const maxHTTPRedirects = 10

// httpClient returns a client that sets headers on every request and
// enforces policy on every request and redirect. It has no overall
// timeout: MCP streams are long-lived, and calls are bounded by their
// contexts instead.
func httpClient(headers map[string]string, policy *security.Policy) *http.Client {
	if policy == nil {
		policy = &security.Policy{}
	}
	proxied := http.DefaultTransport.(*http.Transport).Clone()
	var base http.RoundTripper = &guardTransport{
		policy:  policy,
		proxy:   proxied.Proxy,
		direct:  policy.NewTransport(),
		proxied: proxied,
	}
	if len(headers) > 0 {
		base = &headerTransport{headers: headers, base: base}
	}
	return &http.Client{Transport: base, CheckRedirect: policy.CheckRedirect(maxHTTPRedirects)}
}

// guardTransport sends a request straight to the addresses the policy
// validated for its host (pinned dial). When an environment proxy applies,
// like nanobot it checks the URL and lets the proxy connect, since the
// dialled peer is then the proxy.
type guardTransport struct {
	policy  *security.Policy
	proxy   func(*http.Request) (*url.URL, error)
	direct  http.RoundTripper
	proxied http.RoundTripper
}

func (t *guardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	proxyURL, err := t.proxy(r)
	if err == nil && proxyURL == nil {
		return t.direct.RoundTrip(r)
	}
	if err == nil {
		_, err = t.policy.CheckTarget(r.Context(), r.URL)
	}
	if err != nil {
		if r.Body != nil {
			r.Body.Close() // a RoundTripper must close the body, also on error
		}
		return nil, err
	}
	return t.proxied.RoundTrip(r)
}

type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context()) // a RoundTripper must not modify its request
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return t.base.RoundTrip(r)
}

// tailBuffer keeps the last max bytes written to it, so a failed stdio
// server's last words can go into its error.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(strings.ToValidUTF8(string(b.buf), ""))
}
