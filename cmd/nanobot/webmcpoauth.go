package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/mcp"
)

// mcpActivateTimeout bounds reconnecting a server after its sign-in.
const mcpActivateTimeout = 30 * time.Second

// The WebUI's OAuth sign-in (websocket.MCPOAuth), ported from nanobot's
// webui/mcp_oauth_api.py: a flow connects the saved server alone through
// a throwaway Manager that may send the user through the browser; once
// that stores the credentials, the running gateway reconnects the server
// if it runs it as saved, and otherwise needs a restart.

var errOAuthUnavailable = &websocket.MCPOAuthError{Message: "MCP OAuth is not available.", Status: http.StatusConflict}

// hasCredentials reports whether srv (with ${VAR}s expanded) has stored
// OAuth credentials.
func (s *webMCP) hasCredentials(name string, srv config.MCPServerConfig) bool {
	expanded, err := srv.Expand(s.lookupEnv)
	return err == nil && s.oauth.HasCredentials(name, expanded.URL)
}

// oauthServer loads the config and server name, which must use OAuth.
func (s *webMCP) oauthServer(name string) (*config.Config, config.MCPServerConfig, error) {
	s.settings.mu.Lock()
	cfg, err := config.Load(s.settings.path)
	s.settings.mu.Unlock()
	if err != nil {
		return nil, config.MCPServerConfig{}, err
	}
	srv, ok := cfg.Tools.MCPServers[name]
	if !ok {
		return nil, config.MCPServerConfig{}, websocket.ErrMCPNotFound
	}
	if !srv.UsesOAuth() {
		return nil, config.MCPServerConfig{}, &websocket.MCPOAuthError{Message: "MCP server is not configured for OAuth.", Status: http.StatusConflict}
	}
	return cfg, srv, nil
}

// StartMCPOAuth implements websocket.MCPOAuth.
func (s *webMCP) StartMCPOAuth(name, redirectURI string, reset bool) (websocket.MCPOAuthFlow, error) {
	if s.flows == nil || s.probe == nil {
		return websocket.MCPOAuthFlow{}, errOAuthUnavailable
	}
	_, srv, err := s.oauthServer(name)
	if err != nil {
		return websocket.MCPOAuthFlow{}, err
	}
	if _, err := srv.Expand(s.lookupEnv); err != nil {
		return websocket.MCPOAuthFlow{}, &websocket.MCPOAuthError{Message: "Cannot sign in to " + name + ": " + err.Error() + ".", Status: http.StatusBadRequest}
	}
	return oauthFlowView(s.flows.Start(name, redirectURI, reset))
}

// MCPOAuthStatus implements websocket.MCPOAuth.
func (s *webMCP) MCPOAuthStatus(name, flowID string) (websocket.MCPOAuthFlow, error) {
	if s.flows == nil {
		return websocket.MCPOAuthFlow{}, errOAuthUnavailable
	}
	return oauthFlowView(s.flows.Status(name, flowID))
}

// CompleteMCPOAuth implements websocket.MCPOAuth.
func (s *webMCP) CompleteMCPOAuth(name, flowID, callbackURL string) (websocket.MCPOAuthFlow, error) {
	if s.flows == nil {
		return websocket.MCPOAuthFlow{}, errOAuthUnavailable
	}
	return oauthFlowView(s.flows.CompleteURL(name, flowID, callbackURL))
}

// CancelMCPOAuth implements websocket.MCPOAuth.
func (s *webMCP) CancelMCPOAuth(name, flowID string) (websocket.MCPOAuthFlow, error) {
	if s.flows == nil {
		return websocket.MCPOAuthFlow{}, errOAuthUnavailable
	}
	return oauthFlowView(s.flows.Cancel(name, flowID))
}

// MCPOAuthCallback implements websocket.MCPOAuth.
func (s *webMCP) MCPOAuthCallback(state, code, oauthError, iss string) (string, error) {
	if s.flows == nil {
		return "", errOAuthUnavailable
	}
	name, err := s.flows.Callback(state, code, oauthError, iss)
	return name, oauthErr(err)
}

// connectOAuth is the flows' OAuthConnectFunc: it connects the saved
// server alone, with the browser, until the flow expires. The server is
// signed in even when disabled.
func (s *webMCP) connectOAuth(ctx context.Context, name string, browser *mcp.OAuthBrowser) error {
	cfg, srv, err := s.oauthServer(name)
	if err != nil {
		return err
	}
	srv.Enabled = nil
	cfg.Tools.MCPServers = map[string]config.MCPServerConfig{name: srv}
	m := s.probe(cfg, mcp.WithOAuthStore(s.oauth), mcp.WithOAuthBrowser(name, browser), mcp.WithConnectTimeout(mcp.OAuthFlowTTL))
	defer m.Close()
	if err := m.Connect(ctx); err != nil {
		return err
	}
	if st := m.Status()[0]; st.State != mcp.StateConnected {
		if st.Err != nil {
			return st.Err
		}
		return errors.New(name + " did not connect")
	}
	return nil
}

// activateOAuth is the flows' OAuthActivateFunc: the running gateway
// reconnects the server if it runs it as saved.
func (s *webMCP) activateOAuth(ctx context.Context, name string) mcp.HotReload {
	restart := mcp.HotReload{RequiresRestart: true, Message: "Signed in. Restart the gateway to connect " + name + "."}
	if s.reconnect == nil {
		return restart
	}
	s.settings.mu.Lock()
	cfg, err := config.Load(s.settings.path)
	s.settings.mu.Unlock()
	if err != nil {
		return restart
	}
	saved, ok := cfg.Tools.MCPServers[name]
	running, wasRunning := s.running[name]
	if !ok || !wasRunning || !running.IsEnabled() || !sameMCPServer(saved, ok, running, wasRunning) {
		return restart
	}
	ctx, cancel := context.WithTimeout(ctx, mcpActivateTimeout)
	defer cancel()
	if err := s.reconnect(ctx, name); err != nil {
		return mcp.HotReload{RequiresRestart: true,
			Message: s.scrub("Signed in, but "+name+" could not connect: "+err.Error(), running)}
	}
	return mcp.HotReload{OK: true, Message: name + " connected."}
}

func oauthFlowView(st mcp.OAuthFlowStatus, err error) (websocket.MCPOAuthFlow, error) {
	if err != nil {
		return websocket.MCPOAuthFlow{}, oauthErr(err)
	}
	v := websocket.MCPOAuthFlow{
		FlowID: st.FlowID, Name: st.Name, Status: st.Status, ExpiresIn: st.ExpiresIn,
		CompletionInput: st.CompletionInput, AuthorizationURL: st.AuthorizationURL,
		RedirectURI: st.RedirectURI, Error: st.Error,
	}
	if h := st.HotReload; h != nil {
		v.HotReload = &websocket.MCPOAuthHotReload{OK: h.OK, Message: h.Message, RequiresRestart: h.RequiresRestart}
	}
	return v, nil
}

func oauthErr(err error) error {
	var fe *mcp.OAuthFlowError
	if errors.As(err, &fe) {
		return &websocket.MCPOAuthError{Message: fe.Message, Status: fe.Status}
	}
	return err
}
