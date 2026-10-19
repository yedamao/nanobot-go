package mcp

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/config"
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

// DefaultTransport builds SDK transports from the config: a child process
// for stdio, and HTTP clients that add the configured headers otherwise.
func DefaultTransport(name string, cfg config.MCPServerConfig, stderr io.Writer) (sdk.Transport, error) {
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
		return &sdk.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: httpClient(cfg.Headers)}, nil
	case config.MCPSSE:
		return &sdk.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: httpClient(cfg.Headers)}, nil
	default:
		return nil, fmt.Errorf("unknown MCP transport %q", t)
	}
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

// httpClient returns a client that sets headers on every request. It has
// no overall timeout: MCP streams are long-lived, and calls are bounded by
// their contexts instead.
func httpClient(headers map[string]string) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	if len(headers) == 0 {
		return &http.Client{Transport: base}
	}
	return &http.Client{Transport: &headerTransport{headers: headers, base: base}}
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
