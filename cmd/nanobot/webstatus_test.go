package main

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
)

func TestWebStatus(t *testing.T) {
	const secret = "sk-fake-secret-value"
	ws := filepath.Join(t.TempDir(), "private", "my-workspace")
	cfg := config.Default()
	cfg.Agents.Defaults.Workspace = ws
	cfg.Agents.Defaults.Provider = "anthropic"
	cfg.Agents.Defaults.Model = "claude-test"
	cfg.Providers.Anthropic.APIKey = secret
	cfg.API.Enabled = true
	cfg.API.APIKey = secret
	cfg.Tools.Exec.Enable = false
	cfg.Tools.Web.Enable = true
	cfg.Channels = map[string]json.RawMessage{
		"telegram":     json.RawMessage(`{"enabled": true, "token": "` + secret + `", "allowFrom": ["12345"]}`),
		websocket.Name: json.RawMessage(`{"enabled": true}`),
		"weixin":       json.RawMessage(`{"enabled": false}`),
	}

	started := time.Now().Add(-90 * time.Second)
	running := []string{websocket.Name}
	st := webStatus(cfg, started, func() []string { return running })()

	if st.Version != version || st.Go != runtime.Version() || st.Model != "claude-test" ||
		st.Provider != "anthropic" || st.Workspace != "my-workspace" || !st.APIEnabled ||
		st.Tools != (websocket.ToolStatus{WebFetch: true}) {
		t.Errorf("status = %+v", st)
	}
	if st.UptimeSeconds < 90 || st.StartedAt.IsZero() {
		t.Errorf("started_at = %v, uptime = %d", st.StartedAt, st.UptimeSeconds)
	}
	want := []websocket.ChannelStatus{
		{Name: "telegram", Enabled: true},
		{Name: websocket.Name, Enabled: true, Running: true},
		{Name: "weixin"},
	}
	if !slices.Equal(st.Channels, want) {
		t.Errorf("channels = %+v, want %+v", st.Channels, want)
	}
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secret, "12345", "allowFrom", "private", ws} {
		if strings.Contains(string(data), leak) {
			t.Errorf("status JSON contains %q: %s", leak, data)
		}
	}

	// Running is read on every call.
	running = nil
	if st := webStatus(cfg, started, func() []string { return running })(); st.Channels[1].Running {
		t.Errorf("stopped channel reported running: %+v", st.Channels)
	}
}
