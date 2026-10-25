package main

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
)

// webStatus returns the WebUI's status source (GET /ui/status). The
// config part is taken once, now; each call adds the uptime since started
// and which configured channels running reports. Like `nanobot status`
// it reports names and flags only: the resolved provider preset's name,
// never a key, and the workspace's base name, not its path. Channel
// sections hold tokens and allow lists, so only their "enabled" flag is
// read.
func webStatus(cfg *config.Config, started time.Time, running func() []string) func() websocket.Status {
	d := cfg.Agents.Defaults
	base := websocket.Status{
		Version:    version,
		Go:         runtime.Version(),
		StartedAt:  started.UTC().Truncate(time.Second),
		Model:      d.Model,
		Provider:   d.Provider,
		APIEnabled: cfg.API.Enabled,
		Tools:      websocket.ToolStatus{Exec: cfg.Tools.Exec.Enable, WebFetch: cfg.Tools.Web.Enable},
	}
	if t, err := cfg.Resolve(d.Provider, d.Model); err == nil {
		base.Provider = t.Preset.Name
	}
	if ws, err := cfg.WorkspacePath(); err == nil {
		base.Workspace = filepath.Base(ws)
	}
	var chans []websocket.ChannelStatus
	for _, name := range slices.Sorted(maps.Keys(cfg.Channels)) {
		var head struct {
			Enabled bool `json:"enabled"`
		}
		json.Unmarshal(cfg.Channels[name], &head)
		chans = append(chans, websocket.ChannelStatus{Name: name, Enabled: head.Enabled})
	}
	return func() websocket.Status {
		st := base
		st.UptimeSeconds = int64(time.Since(started) / time.Second)
		st.Channels = slices.Clone(chans)
		up := running()
		for i := range st.Channels {
			st.Channels[i].Running = slices.Contains(up, st.Channels[i].Name)
		}
		return st
	}
}
