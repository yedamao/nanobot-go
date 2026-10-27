package websocket

import (
	"errors"
	"html"
	"net/http"
	"strings"
)

// MCP OAuth sign-in, as nanobot's /api/settings/mcp-oauth/* routes:
//
//	POST /ui/mcp/{name}/oauth/start    {"reset":bool}  start a browser flow
//	GET  /ui/mcp/{name}/oauth/status   ?flow_id=       poll it
//	POST /ui/mcp/{name}/oauth/complete {"flow_id","callback_url"}  paste the callback
//	POST /ui/mcp/{name}/oauth/cancel   {"flow_id"}     stop it
//	GET  /auth/mcp/callback            the authorization server's redirect
const (
	MCPOAuthStartPath    = MCPPath + "/{name}/oauth/start"
	MCPOAuthStatusPath   = MCPPath + "/{name}/oauth/status"
	MCPOAuthCompletePath = MCPPath + "/{name}/oauth/complete"
	MCPOAuthCancelPath   = MCPPath + "/{name}/oauth/cancel"
	// MCPOAuthCallbackPath must match the redirect URI registered with
	// authorization servers (nanobot's MCP_OAUTH_CALLBACK_PATH).
	MCPOAuthCallbackPath = "/auth/mcp/callback"
	// maxMCPOAuthCallbackURL caps a pasted callback URL.
	maxMCPOAuthCallbackURL = 16 << 10
)

// MCPOAuth runs browser sign-ins for OAuth MCP servers. Errors are
// ErrMCPNotFound, ErrInvalidMCP or *MCPOAuthError (shown as is);
// anything else is answered with a generic 500.
type MCPOAuth interface {
	// StartMCPOAuth starts a flow whose callback is redirectURI, derived
	// from the WebUI's origin.
	StartMCPOAuth(name, redirectURI string, reset bool) (MCPOAuthFlow, error)
	MCPOAuthStatus(name, flowID string) (MCPOAuthFlow, error)
	CompleteMCPOAuth(name, flowID, callbackURL string) (MCPOAuthFlow, error)
	CancelMCPOAuth(name, flowID string) (MCPOAuthFlow, error)
	// MCPOAuthCallback delivers the callback's query and returns the
	// server name.
	MCPOAuthCallback(state, code, oauthError, iss string) (string, error)
}

// MCPOAuthError is a safe message with its HTTP status.
type MCPOAuthError struct {
	Message string
	Status  int
}

func (e *MCPOAuthError) Error() string { return e.Message }

// MCPOAuthFlow is a sign-in attempt as the page sees it. Status is
// starting, authorization_required (open AuthorizationURL), connecting,
// authorized, connected, failed or cancelled.
type MCPOAuthFlow struct {
	FlowID    string `json:"flow_id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	ExpiresIn int    `json:"expires_in"`
	// CompletionInput is "callback_url" when the page must ask for the
	// URL the browser lands on (RedirectURI, a loopback address).
	CompletionInput  string             `json:"completion_input,omitempty"`
	AuthorizationURL string             `json:"authorization_url,omitempty"`
	RedirectURI      string             `json:"redirect_uri,omitempty"`
	Error            string             `json:"error,omitempty"`
	HotReload        *MCPOAuthHotReload `json:"hot_reload,omitempty"`
}

// MCPOAuthHotReload reports activating the server after its sign-in.
type MCPOAuthHotReload struct {
	OK              bool   `json:"ok"`
	Message         string `json:"message,omitempty"`
	RequiresRestart bool   `json:"requires_restart,omitempty"`
}

type mcpOAuthStartBody struct {
	Reset bool `json:"reset"`
}

type mcpOAuthFlowBody struct {
	FlowID      string `json:"flow_id"`
	CallbackURL string `json:"callback_url"`
}

// mcpOAuthRedirectURI is the callback on the origin the page was loaded
// from.
func mcpOAuthRedirectURI(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + MCPOAuthCallbackPath
}

func (c *Channel) startMCPOAuth(w http.ResponseWriter, r *http.Request) {
	var body mcpOAuthStartBody
	if r.ContentLength != 0 {
		if err := decodeJSONBody(r, &body, 1<<10); err != nil {
			http.Error(w, `body must be {"reset":true|false}`, http.StatusBadRequest)
			return
		}
	}
	flow, err := c.mcpOAuth.StartMCPOAuth(r.PathValue("name"), mcpOAuthRedirectURI(r), body.Reset)
	c.mcpOAuthReply(w, "start", flow, err)
}

func (c *Channel) mcpOAuthStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("flow_id"))
	if id == "" {
		http.Error(w, "missing MCP OAuth flow ID", http.StatusBadRequest)
		return
	}
	flow, err := c.mcpOAuth.MCPOAuthStatus(r.PathValue("name"), id)
	c.mcpOAuthReply(w, "status", flow, err)
}

func (c *Channel) completeMCPOAuth(w http.ResponseWriter, r *http.Request) {
	var body mcpOAuthFlowBody
	if err := decodeJSONBody(r, &body, maxMCPOAuthCallbackURL+1<<10); err != nil {
		http.Error(w, `body must be {"flow_id":"...","callback_url":"..."}`, http.StatusBadRequest)
		return
	}
	id, cb := strings.TrimSpace(body.FlowID), strings.TrimSpace(body.CallbackURL)
	switch {
	case id == "":
		http.Error(w, "missing MCP OAuth flow ID", http.StatusBadRequest)
		return
	case cb == "":
		http.Error(w, "Paste the complete callback URL to continue.", http.StatusBadRequest)
		return
	case len(cb) > maxMCPOAuthCallbackURL:
		http.Error(w, "The MCP OAuth callback URL is too long.", http.StatusBadRequest)
		return
	}
	flow, err := c.mcpOAuth.CompleteMCPOAuth(r.PathValue("name"), id, cb)
	c.mcpOAuthReply(w, "complete", flow, err)
}

func (c *Channel) cancelMCPOAuth(w http.ResponseWriter, r *http.Request) {
	var body mcpOAuthFlowBody
	if err := decodeJSONBody(r, &body, 1<<10); err != nil || strings.TrimSpace(body.FlowID) == "" {
		http.Error(w, "missing MCP OAuth flow ID", http.StatusBadRequest)
		return
	}
	flow, err := c.mcpOAuth.CancelMCPOAuth(r.PathValue("name"), strings.TrimSpace(body.FlowID))
	c.mcpOAuthReply(w, "cancel", flow, err)
}

func (c *Channel) mcpOAuthReply(w http.ResponseWriter, op string, flow MCPOAuthFlow, err error) {
	var oe *MCPOAuthError
	switch {
	case err == nil:
		writeJSON(w, flow)
	case errors.As(err, &oe) && oe.Status >= 400 && oe.Status < 500:
		http.Error(w, oe.Message, oe.Status)
	default:
		c.mcpError(w, "run the OAuth "+op+" of", err)
	}
}

// mcpOAuthCallback answers GET /auth/mcp/callback, where the browser
// lands after authorizing: a small page saying whether to go back to the
// WebUI. The state identifies the flow, so no sign-in is needed.
func (c *Channel) mcpOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	if state == "" {
		mcpOAuthPage(w, http.StatusBadRequest, false, "This authorization request is missing its security state.")
		return
	}
	name, err := c.mcpOAuth.MCPOAuthCallback(state, q.Get("code"), q.Get("error"), q.Get("iss"))
	if err != nil {
		status, msg := http.StatusBadRequest, "Could not complete MCP authorization."
		var oe *MCPOAuthError
		if errors.As(err, &oe) {
			status, msg = oe.Status, oe.Message
		}
		mcpOAuthPage(w, status, false, msg)
		return
	}
	mcpOAuthPage(w, http.StatusOK, true, "Authorization received for "+name+". Return to nanobot-go to finish connecting.")
}

func mcpOAuthPage(w http.ResponseWriter, status int, ok bool, message string) {
	title := "Connection failed"
	script := ""
	if ok {
		title = "Authorization received"
		script = "<script>setTimeout(() => window.close(), 700)</script>"
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
	w.WriteHeader(status)
	_, _ = w.Write([]byte("<!doctype html><html><head><meta charset='utf-8'>" +
		"<meta name='viewport' content='width=device-width,initial-scale=1'>" +
		"<title>" + html.EscapeString(title) + "</title><style>" +
		"body{font:16px system-ui;margin:0;min-height:100vh;display:grid;place-items:center;" +
		"background:#f7f7f6;color:#171717}.card{max-width:34rem;margin:2rem;padding:2rem;" +
		"border:1px solid #ddd;border-radius:16px;background:white}h1{font-size:1.35rem}" +
		"p{line-height:1.55;color:#555}</style></head><body><main class='card'>" +
		"<h1>" + html.EscapeString(title) + "</h1><p>" + html.EscapeString(message) + "</p></main>" +
		script + "</body></html>"))
}
