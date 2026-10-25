package websocket

import (
	"errors"
	"net/http"
)

const (
	// MCPPath lists the configured MCP servers (GET /ui/mcp); PUT, PATCH
	// and DELETE /ui/mcp/{name} create or replace, enable/disable and
	// remove one (tools.mcpServers in the config file).
	MCPPath = "/ui/mcp"
	// maxMCPBody caps the JSON body of an MCP server mutation.
	maxMCPBody = 64 << 10
)

var (
	// ErrInvalidMCP marks an MCPStore error caused by the requested
	// change; it is answered with 400 and the error's text.
	ErrInvalidMCP = errors.New("invalid MCP server")
	// ErrMCPNotFound marks an MCPStore error for a server not in the
	// config; it is answered with 404.
	ErrMCPNotFound = errors.New("no such MCP server")
)

// MCPStore reads and changes tools.mcpServers. Implementations must load
// the config fresh from disk for every change and write back only that
// section, and must never return env or header values.
type MCPStore interface {
	MCPServers() (MCPList, error)
	// PutMCPServer creates or replaces a server; created reports which.
	PutMCPServer(name string, in MCPServerInput) (view MCPServerView, created bool, err error)
	SetMCPServerEnabled(name string, enabled bool) (MCPServerView, error)
	DeleteMCPServer(name string) error
}

// MCPList is the body of GET /ui/mcp.
type MCPList struct {
	Servers []MCPServerView `json:"servers"`
	// Live reports whether the running gateway's connection states are
	// included; without it every state is "unknown".
	Live bool `json:"live"`
	// RestartRequired names the servers whose saved config differs from
	// the one the gateway runs with, including removed ones. MCP servers
	// connect when the gateway starts, so changes need a restart.
	RestartRequired []string `json:"restart_required"`
}

// MCPServerView is one server as the page sees it. Env and header values,
// URL credentials and query strings, and secret-looking args are never
// included: only env and header names.
type MCPServerView struct {
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Transport string `json:"transport"`
	// Type is the configured type, empty when inferred.
	Type    string   `json:"type,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	URL     string   `json:"url,omitempty"`
	// EnvKeys and HeaderKeys are the names of the configured env
	// variables and headers, sorted.
	EnvKeys    []string `json:"env_keys,omitempty"`
	HeaderKeys []string `json:"header_keys,omitempty"`
	// ToolTimeout is the per-call timeout in seconds (0 means default).
	ToolTimeout int `json:"tool_timeout,omitempty"`
	// EnabledTools is the tool whitelist; null exposes every tool.
	EnabledTools []string `json:"enabled_tools"`
	// State is the running gateway's connection state: connected,
	// failed, disabled, pending, closed, "not_running" for a server the
	// gateway started without, or "unknown" when not Live.
	State string `json:"state"`
	// Error is the last connection error, with secrets removed.
	Error string `json:"error,omitempty"`
	// Tools are the registered tool names when connected.
	Tools []string `json:"tools,omitempty"`
	// RestartRequired reports that the saved config differs from the
	// running one.
	RestartRequired bool `json:"restart_required"`
	// Removed marks a server the gateway runs that is no longer saved.
	Removed bool `json:"removed,omitempty"`
}

// MCPServerInput is the body of PUT /ui/mcp/{name}; it mirrors
// tools.mcpServers.<name>. When replacing a server, a missing env or
// headers keeps the saved values, so the page can edit a server without
// ever seeing them; an object (even {}) replaces them.
type MCPServerInput struct {
	Type         string             `json:"type"`
	Command      string             `json:"command"`
	Args         []string           `json:"args"`
	Env          *map[string]string `json:"env"`
	Cwd          string             `json:"cwd"`
	URL          string             `json:"url"`
	Headers      *map[string]string `json:"headers"`
	ToolTimeout  int                `json:"toolTimeout"`
	EnabledTools []string           `json:"enabledTools"`
	Enabled      *bool              `json:"enabled"`
}

// serveMCP answers GET /ui/mcp.
func (c *Channel) serveMCP(w http.ResponseWriter, _ *http.Request) {
	list, err := c.mcp.MCPServers()
	if err != nil {
		c.mcpError(w, "list", err)
		return
	}
	writeJSON(w, list)
}

// putMCP answers PUT /ui/mcp/{name}: 201 on create, 200 on replace.
func (c *Channel) putMCP(w http.ResponseWriter, r *http.Request) {
	var in MCPServerInput
	if err := decodeJSONBody(r, &in, maxMCPBody); err != nil {
		http.Error(w, "body must be an MCP server object (type, command, args, env, cwd, url, headers, toolTimeout, enabledTools, enabled)", http.StatusBadRequest)
		return
	}
	v, created, err := c.mcp.PutMCPServer(r.PathValue("name"), in)
	if err != nil {
		c.mcpError(w, "save", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSONStatus(w, status, v)
}

type mcpEnabledBody struct {
	Enabled *bool `json:"enabled"`
}

// patchMCP answers PATCH /ui/mcp/{name} {"enabled":bool}.
func (c *Channel) patchMCP(w http.ResponseWriter, r *http.Request) {
	var body mcpEnabledBody
	if err := decodeJSONBody(r, &body, maxMCPBody); err != nil || body.Enabled == nil {
		http.Error(w, `body must be {"enabled":true|false}`, http.StatusBadRequest)
		return
	}
	v, err := c.mcp.SetMCPServerEnabled(r.PathValue("name"), *body.Enabled)
	if err != nil {
		c.mcpError(w, "update", err)
		return
	}
	writeJSON(w, v)
}

// deleteMCP answers DELETE /ui/mcp/{name}.
func (c *Channel) deleteMCP(w http.ResponseWriter, r *http.Request) {
	if err := c.mcp.DeleteMCPServer(r.PathValue("name")); err != nil {
		c.mcpError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mcpError answers a failed MCP request. Only ErrInvalidMCP errors are
// shown; they name fields, never their values.
func (c *Channel) mcpError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, ErrInvalidMCP):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrMCPNotFound):
		http.Error(w, "no such MCP server", http.StatusNotFound)
	default:
		c.log.Warn("websocket: "+op+" MCP server", "error", err)
		http.Error(w, "cannot "+op+" the MCP server", http.StatusInternalServerError)
	}
}
