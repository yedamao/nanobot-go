package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/mcp"
	"github.com/yedamao/nanobot-go/internal/tools"
)

const linearURL = "https://mcp.example.test/linear"

// oauthMCP returns an MCP store whose config adds the OAuth server
// "linear" (running as saved), with credentials in the data directory.
func oauthMCP(t *testing.T, reconnect func(context.Context, string) error) (*webMCP, *mcp.OAuthStore, string) {
	t.Helper()
	path := writeSettingsConfig(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Tools.MCPServers["linear"] = config.MCPServerConfig{URL: linearURL, Auth: config.MCPAuthOAuth}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	s := runningMCP(t, path, func() []mcp.ServerStatus {
		return []mcp.ServerStatus{{Name: "linear", State: mcp.StateAuthorizationRequired, Err: mcp.ErrAuthorizationRequired}}
	})
	s.probe = func(cfg *config.Config, opts ...mcp.Option) *mcp.Manager {
		return mcp.NewManager(cfg.Tools.MCPServers, tools.NewRegistry(), nil, opts...)
	}
	store := mcp.NewOAuthStore(config.MCPOAuthPath(path), nil)
	flows := s.enableOAuth(store, reconnect, nil)
	t.Cleanup(flows.Close)
	return s, store, path
}

// seedCredentials writes a token for name at url as nanobot's store
// holds it.
func seedCredentials(t *testing.T, path, name, url string) {
	t.Helper()
	sum := sha256.Sum256([]byte(url))
	data := `{"version":1,"servers":{"` + name + `":{"server_fingerprint":"` + hex.EncodeToString(sum[:]) + `","tokens":{"access_token":"tok","token_type":"Bearer"}}},"generations":{}}`
	p := config.MCPOAuthPath(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func viewOf(t *testing.T, s *webMCP, name string) websocket.MCPServerView {
	t.Helper()
	list, err := s.MCPServers()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range list.Servers {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no server %s in %+v", name, list.Servers)
	return websocket.MCPServerView{}
}

func TestWebMCPOAuthView(t *testing.T) {
	s, store, path := oauthMCP(t, nil)
	v := viewOf(t, s, "linear")
	if v.Auth != "oauth" || v.OAuth != mcp.OAuthAuthorizationRequired || v.State != string(mcp.StateAuthorizationRequired) {
		t.Errorf("linear = %+v", v)
	}
	if v := viewOf(t, s, "srv"); v.Auth != "" || v.OAuth != "" {
		t.Errorf("header server = %+v", v)
	}
	seedCredentials(t, path, "linear", linearURL)
	if !store.HasCredentials("linear", linearURL) {
		t.Fatal("seeded credentials not read")
	}
	if v := viewOf(t, s, "linear"); v.OAuth != mcp.OAuthAuthorized {
		t.Errorf("after sign-in: %+v", v)
	}
}

func TestWebMCPOAuthCredentialsFollowConfig(t *testing.T) {
	s, store, path := oauthMCP(t, nil)
	seedCredentials(t, path, "linear", linearURL)

	// Replacing the server as it was keeps the credentials.
	if _, _, err := s.PutMCPServer("linear", websocket.MCPServerInput{URL: linearURL, Auth: "oauth"}); err != nil {
		t.Fatal(err)
	}
	if !store.HasCredentials("linear", linearURL) {
		t.Fatal("unchanged server lost its credentials")
	}
	// Moving it drops them.
	if _, _, err := s.PutMCPServer("linear", websocket.MCPServerInput{URL: linearURL + "/v2", Auth: "oauth"}); err != nil {
		t.Fatal(err)
	}
	if store.HasCredentials("linear", linearURL) {
		t.Error("moved server kept its credentials")
	}
	// Turning OAuth off drops them.
	seedCredentials(t, path, "linear", linearURL+"/v2")
	if _, _, err := s.PutMCPServer("linear", websocket.MCPServerInput{URL: linearURL + "/v2"}); err != nil {
		t.Fatal(err)
	}
	if store.HasCredentials("linear", linearURL+"/v2") {
		t.Error("OAuth turned off kept the credentials")
	}
	// Deleting drops them.
	if _, _, err := s.PutMCPServer("linear", websocket.MCPServerInput{URL: linearURL, Auth: "oauth"}); err != nil {
		t.Fatal(err)
	}
	seedCredentials(t, path, "linear", linearURL)
	if err := s.DeleteMCPServer("linear"); err != nil {
		t.Fatal(err)
	}
	if store.HasCredentials("linear", linearURL) {
		t.Error("deleted server kept its credentials")
	}
	// OAuth needs an HTTP server.
	if _, _, err := s.PutMCPServer("local", websocket.MCPServerInput{Command: "srv", Auth: "oauth"}); !errors.Is(err, websocket.ErrInvalidMCP) {
		t.Errorf("stdio oauth = %v", err)
	}
	if _, _, err := s.PutMCPServer("odd", websocket.MCPServerInput{URL: linearURL, Auth: "basic"}); !errors.Is(err, websocket.ErrInvalidMCP) {
		t.Errorf("unknown auth = %v", err)
	}
}

func TestWebMCPOAuthStartChecks(t *testing.T) {
	s, _, _ := oauthMCP(t, nil)
	var oe *websocket.MCPOAuthError
	if _, err := s.StartMCPOAuth("srv", "http://127.0.0.1:18790/auth/mcp/callback", false); !errors.As(err, &oe) || oe.Status != http.StatusConflict {
		t.Errorf("header server = %v", err)
	}
	if _, err := s.StartMCPOAuth("nope", "http://127.0.0.1:18790/auth/mcp/callback", false); !errors.Is(err, websocket.ErrMCPNotFound) {
		t.Errorf("unknown = %v", err)
	}
	if _, err := s.StartMCPOAuth("linear", "http://127.0.0.1:18790/elsewhere", false); !errors.As(err, &oe) || oe.Status != http.StatusBadRequest {
		t.Errorf("bad redirect = %v", err)
	}
	if _, err := s.MCPOAuthStatus("linear", "nope"); !errors.As(err, &oe) || oe.Status != http.StatusNotFound {
		t.Errorf("unknown flow = %v", err)
	}
	if _, err := s.MCPOAuthCallback("nope", "c", "", ""); !errors.As(err, &oe) || oe.Status != http.StatusGone {
		t.Errorf("unknown state = %v", err)
	}
}

func TestWebMCPOAuthActivate(t *testing.T) {
	ctx := context.Background()
	s, _, _ := oauthMCP(t, nil)
	if h := s.activateOAuth(ctx, "linear"); h.OK || !h.RequiresRestart {
		t.Errorf("without a running Manager: %+v", h)
	}

	var reconnected []string
	s, _, _ = oauthMCP(t, func(_ context.Context, name string) error {
		reconnected = append(reconnected, name)
		return nil
	})
	if h := s.activateOAuth(ctx, "linear"); !h.OK || h.RequiresRestart {
		t.Errorf("running as saved: %+v", h)
	}
	if len(reconnected) != 1 || reconnected[0] != "linear" {
		t.Errorf("reconnected %v", reconnected)
	}
	// Saved differently from how it runs: restart.
	if _, _, err := s.PutMCPServer("linear", websocket.MCPServerInput{URL: linearURL + "/v2", Auth: "oauth"}); err != nil {
		t.Fatal(err)
	}
	if h := s.activateOAuth(ctx, "linear"); h.OK || !h.RequiresRestart {
		t.Errorf("changed since start: %+v", h)
	}
	if len(reconnected) != 1 {
		t.Errorf("reconnected a changed server: %v", reconnected)
	}
}
