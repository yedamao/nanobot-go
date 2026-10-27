package mcp

import (
	"context"
	"errors"
	"slices"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yedamao/nanobot-go/internal/config"
)

func (h *harness) reload(t *testing.T, next map[string]config.MCPServerConfig) ReloadResult {
	t.Helper()
	res, err := h.mgr.Reload(context.Background(), next)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	return res
}

func oneToolServer() *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "one", Version: "1"}, nil)
	srv.AddTool(&sdk.Tool{Name: "only", InputSchema: objectSchema},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) { return textResult("v2"), nil })
	return srv
}

func TestReloadAddChangeRemove(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio(), "beta": stdio(), "gamma": stdio()})
	h.connect(t)
	h.fakes.build["delta"] = testServer
	h.fakes.build["beta"] = oneToolServer

	changed := stdio()
	changed.Args = []string{"--v2"}
	res := h.reload(t, map[string]config.MCPServerConfig{"alpha": stdio(), "beta": changed, "delta": stdio()})
	if !res.OK || res.RequiresRestart || res.Message != "MCP config reloaded without restarting the gateway." {
		t.Errorf("result = %+v", res)
	}
	if !slices.Equal(res.Added, []string{"delta"}) || !slices.Equal(res.Changed, []string{"beta"}) ||
		!slices.Equal(res.Removed, []string{"gamma"}) || len(res.Retried) != 0 || len(res.Failed) != 0 {
		t.Errorf("diff = %+v", res)
	}
	if res.ToolsRemoved != 8 || !slices.Equal(res.Connected, []string{"alpha", "beta", "delta"}) ||
		!slices.Equal(res.Configured, []string{"alpha", "beta", "delta"}) {
		t.Errorf("tools removed %d, connected %v, configured %v", res.ToolsRemoved, res.Connected, res.Configured)
	}
	if n := h.fakes.dialCount("alpha"); n != 1 {
		t.Errorf("unchanged alpha dialed %d times", n)
	}
	want := []string{
		"mcp_alpha_add", "mcp_alpha_echo", "mcp_alpha_fail", "mcp_alpha_sleep",
		"mcp_beta_only",
		"mcp_delta_add", "mcp_delta_echo", "mcp_delta_fail", "mcp_delta_sleep",
	}
	if got := h.reg.Names(); !slices.Equal(got, want) {
		t.Errorf("registered %v, want %v", got, want)
	}
	if r := h.exec("mcp_beta_only", `{}`); r.IsError || r.Content != "v2" {
		t.Errorf("changed server call = %+v", r)
	}
	var names []string
	for _, st := range h.mgr.Status() {
		names = append(names, st.Name)
	}
	if !slices.Equal(names, []string{"alpha", "beta", "delta"}) {
		t.Errorf("status names %v", names)
	}

	if res := h.reload(t, map[string]config.MCPServerConfig{"alpha": stdio(), "beta": changed, "delta": stdio()}); !res.OK || res.Message != "MCP config is already live." {
		t.Errorf("no-op reload = %+v", res)
	}
}

func TestReloadDisableEnable(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio()})
	h.connect(t)
	off := stdio()
	off.Enabled = new(bool)
	res := h.reload(t, map[string]config.MCPServerConfig{"alpha": off})
	if !res.OK || !slices.Equal(res.Changed, []string{"alpha"}) || res.ToolsRemoved != 4 || len(res.Connected) != 0 {
		t.Errorf("disable = %+v", res)
	}
	if st := h.status(t, "alpha"); st.State != StateDisabled || len(h.reg.Names()) != 0 {
		t.Errorf("disabled: %s, tools %v", st.State, h.reg.Names())
	}
	if err := h.mgr.Reconnect(context.Background(), "alpha"); err == nil {
		t.Error("Reconnect of a disabled server succeeded")
	}
	res = h.reload(t, map[string]config.MCPServerConfig{"alpha": stdio()})
	if !res.OK || !slices.Equal(res.Connected, []string{"alpha"}) || len(h.reg.Names()) != 4 {
		t.Errorf("enable = %+v, tools %v", res, h.reg.Names())
	}
}

func TestReloadRetriesFailed(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio()})
	h.fakes.fail["alpha"] = errors.New("boom")
	h.connect(t)
	if st := h.status(t, "alpha"); st.State != StateFailed {
		t.Fatalf("state = %s", st.State)
	}
	res := h.reload(t, map[string]config.MCPServerConfig{"alpha": stdio()})
	if res.OK || !slices.Equal(res.Failed, []string{"alpha"}) || !slices.Equal(res.Retried, []string{"alpha"}) ||
		res.Message != "MCP config reloaded, but some servers did not connect: alpha" {
		t.Errorf("still failing = %+v", res)
	}
	delete(h.fakes.fail, "alpha")
	res = h.reload(t, map[string]config.MCPServerConfig{"alpha": stdio()})
	if !res.OK || !slices.Equal(res.Retried, []string{"alpha"}) || res.Message != "MCP connections refreshed without restarting the gateway." {
		t.Errorf("retry = %+v", res)
	}
	if st := h.status(t, "alpha"); st.State != StateConnected {
		t.Errorf("state = %s", st.State)
	}
}

// A tool of a replaced server, held by a caller across the reload, does
// not reconnect the old config.
func TestReloadRetiresOldTools(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio()})
	h.connect(t)
	old, _ := h.reg.Get("mcp_alpha_echo")
	changed := stdio()
	changed.ToolTimeout = 5
	h.reload(t, map[string]config.MCPServerConfig{"alpha": changed})
	if r := old.Execute(context.Background(), []byte(`{"text":"x"}`)); !r.IsError {
		t.Errorf("stale tool call = %+v", r)
	}
	if n := h.fakes.dialCount("alpha"); n != 2 {
		t.Errorf("dials = %d, want 2", n)
	}
}

// OAuth servers without credentials are not dialed; once signed in, a
// reload connects them.
func TestReloadSkipsOAuthPending(t *testing.T) {
	idp := newFakeIdP(t)
	st := newTestStore(t)
	cfg := config.MCPServerConfig{URL: idp.srv.URL + "/mcp", Auth: config.MCPAuthOAuth}
	m := oauthManager(t, st, nil)
	res, err := m.Reload(context.Background(), map[string]config.MCPServerConfig{"srv": cfg})
	if err != nil || !res.OK || !slices.Equal(res.Added, []string{"srv"}) || len(res.Failed) != 0 || len(res.Connected) != 0 {
		t.Fatalf("pending = %+v, %v", res, err)
	}
	if got := m.Status()[0]; got.State != StateAuthorizationRequired {
		t.Errorf("state = %s", got.State)
	}
	if n := idp.requests.Load(); n != 0 {
		t.Errorf("%d requests reached the server", n)
	}
	seedAuthorized(t, idp, st, cfg)
	res, err = m.Reload(context.Background(), map[string]config.MCPServerConfig{"srv": cfg})
	if err != nil || !res.OK || !slices.Equal(res.Retried, []string{"srv"}) || !slices.Equal(res.Connected, []string{"srv"}) {
		t.Errorf("after sign-in = %+v, %v", res, err)
	}
}

func TestReloadAfterClose(t *testing.T) {
	h := newHarness(t, map[string]config.MCPServerConfig{"alpha": stdio()})
	h.connect(t)
	_ = h.mgr.Close()
	res, err := h.mgr.Reload(context.Background(), map[string]config.MCPServerConfig{"beta": stdio()})
	if err == nil || res.OK || !res.RequiresRestart {
		t.Errorf("closed = %+v, %v", res, err)
	}
	if n := h.fakes.dialCount("beta"); n != 0 {
		t.Errorf("closed Manager dialed beta")
	}
}
