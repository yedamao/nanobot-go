package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/tools"
)

// remoteTool exposes one server tool. It holds no session: every call
// asks the Manager for the server's current one, so a reconnect needs no
// re-registration.
type remoteTool struct {
	m        *Manager
	srv      *server
	name     string // mcp_<server>_<tool>
	remote   string // the tool's name on the server
	desc     string
	params   map[string]any
	readOnly bool
	timeout  time.Duration
}

func (m *Manager) newTool(s *server, name string, d *sdk.Tool) *remoteTool {
	desc := d.Description
	if desc == "" {
		desc = d.Title
	}
	if desc == "" {
		desc = d.Name
	}
	return &remoteTool{
		m:      m,
		srv:    s,
		name:   name,
		remote: d.Name,
		desc:   fmt.Sprintf("[MCP %s] %s", s.name, desc),
		params: normalizeSchema(d.InputSchema, m.logger.With("server", s.name, "tool", d.Name)),
		// Annotations are hints from a possibly untrusted server; trusting
		// readOnlyHint only lets the call run alongside other read-only
		// tools.
		readOnly: d.Annotations != nil && d.Annotations.ReadOnlyHint,
		timeout:  s.cfg.Timeout(),
	}
}

func (t *remoteTool) Name() string               { return t.name }
func (t *remoteTool) Description() string        { return t.desc }
func (t *remoteTool) Parameters() map[string]any { return t.params }
func (t *remoteTool) ReadOnly() bool             { return t.readOnly }

// Execute calls the tool. If the session turns out to be dead, it
// reconnects once and retries. Failures become error results.
func (t *remoteTool) Execute(ctx context.Context, args json.RawMessage) tools.Result {
	c, err := t.m.session(ctx, t.srv)
	if err != nil {
		return t.unavailable(err)
	}
	res, err := t.call(ctx, c, args)
	// A call canceled by c's lifetime ending (Close, or the session dying)
	// counts as a dead session too; reconnecting fails fast after Close.
	if err != nil && ctx.Err() == nil && (sessionDead(err) || c.ctx.Err() != nil) {
		t.m.logger.Warn("MCP session lost; reconnecting", "server", t.srv.name, "tool", t.name, "error", err)
		if c, err = t.m.connect(ctx, t.srv, c); err != nil {
			return t.unavailable(err)
		}
		res, err = t.call(ctx, c, args)
	}
	switch {
	case err == nil:
		return render(res)
	case ctx.Err() != nil:
		return tools.Errorf("MCP tool %s canceled: %v", t.name, ctx.Err())
	case errors.Is(err, context.DeadlineExceeded):
		return tools.Errorf("MCP tool %s timed out after %s", t.name, t.timeout)
	}
	t.m.logger.Warn("MCP tool call failed", "server", t.srv.name, "tool", t.name, "error", err)
	return tools.Errorf("MCP tool %s failed: %v", t.name, err)
}

func (t *remoteTool) call(ctx context.Context, c *conn, args json.RawMessage) (*sdk.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	defer context.AfterFunc(c.ctx, cancel)()
	return c.session.CallTool(ctx, &sdk.CallToolParams{Name: t.remote, Arguments: args})
}

func (t *remoteTool) unavailable(err error) tools.Result {
	if errors.Is(err, errClosed) {
		return tools.Errorf("MCP server %q is shut down", t.srv.name)
	}
	return tools.Errorf("MCP server %q is unavailable: %v", t.srv.name, err)
}

// sessionDead reports whether err means the connection is gone (server
// exited, pipe or socket broken, HTTP session expired), as opposed to the
// server rejecting the call.
func sessionDead(err error) bool {
	for _, target := range []error{
		sdk.ErrConnectionClosed, sdk.ErrSessionMissing,
		io.EOF, io.ErrUnexpectedEOF, io.ErrClosedPipe, net.ErrClosed,
		syscall.EPIPE, syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ECONNABORTED,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// render turns a call result into text. Binary content is described, not
// inlined: base64 would be truncated and waste context.
func render(res *sdk.CallToolResult) tools.Result {
	var parts []string
	for _, c := range res.Content {
		switch x := c.(type) {
		case *sdk.TextContent:
			parts = append(parts, x.Text)
		case *sdk.ImageContent:
			parts = append(parts, fmt.Sprintf("[image: %s, %d bytes]", orUnknown(x.MIMEType), len(x.Data)))
		case *sdk.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio: %s, %d bytes]", orUnknown(x.MIMEType), len(x.Data)))
		case *sdk.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource link: %s %s]", x.Name, x.URI))
		case *sdk.EmbeddedResource:
			parts = append(parts, renderResource(x.Resource))
		default:
			parts = append(parts, fmt.Sprintf("[unsupported content %T]", c))
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if data, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(data))
		}
	}
	out := strings.Join(parts, "\n")
	if out == "" {
		out = "(no output)"
	}
	return tools.Result{Content: out, IsError: res.IsError}
}

func renderResource(r *sdk.ResourceContents) string {
	switch {
	case r == nil:
		return "[empty resource]"
	case r.Text != "":
		return r.Text
	}
	return fmt.Sprintf("[resource: %s, %s, %d bytes]", r.URI, orUnknown(r.MIMEType), len(r.Blob))
}

func orUnknown(mime string) string {
	if mime == "" {
		return "unknown type"
	}
	return mime
}
