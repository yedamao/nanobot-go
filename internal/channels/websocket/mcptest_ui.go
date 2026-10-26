package websocket

import (
	"context"
	"net/http"
)

// MCPTestPath is POST /ui/mcp/{name}/test: probe whether a saved MCP
// server connects and lists its tools. Nothing is saved and the running
// gateway's connections are left alone.
const MCPTestPath = MCPPath + "/{name}/test"

// MCP test statuses, as for providers.
const (
	// MCPTestOK: the server completed the handshake and listed its tools.
	MCPTestOK = ProviderTestOK
	// MCPTestNotConfigured: nothing to probe (disabled, or a ${VAR} it
	// needs is unset).
	MCPTestNotConfigured = ProviderTestNotConfigured
	// MCPTestRejected: the server answered 401 or 403.
	MCPTestRejected = ProviderTestRejected
	// MCPTestError: the probe failed otherwise (missing command, network,
	// SSRF block, timeout, handshake).
	MCPTestError = ProviderTestError
)

// MCPTester probes a saved MCP server. It returns ErrMCPNotFound for a
// server not in the config. A completed probe, failed or not, is a
// result, not an error.
type MCPTester interface {
	TestMCPServer(ctx context.Context, name string) (MCPTestResult, error)
}

// MCPTestResult is the answer of POST /ui/mcp/{name}/test. Message is
// short, safe text: never an env or header value, URL credentials or a
// secret-looking arg.
type MCPTestResult struct {
	OK      bool   `json:"ok"`
	Status  string `json:"status"`
	Message string `json:"message"`
	// Tools are the model-facing tool names the server offers, ignoring
	// enabledTools, when OK.
	Tools []string `json:"tools,omitempty"`
}

// testMCP answers POST /ui/mcp/{name}/test. Any body is ignored: the
// saved config is tested.
func (c *Channel) testMCP(w http.ResponseWriter, r *http.Request) {
	res, err := c.mcpTester.TestMCPServer(r.Context(), r.PathValue("name"))
	if err != nil {
		c.mcpError(w, "test", err)
		return
	}
	writeJSON(w, res)
}
