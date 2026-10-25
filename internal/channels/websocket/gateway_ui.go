package websocket

import (
	"errors"
	"net/http"
)

const (
	// GatewayPath reports how the gateway serving the page was launched
	// and whether the page can restart it (GET /ui/gateway).
	GatewayPath = "/ui/gateway"
	// GatewayRestartPath restarts it (POST /ui/gateway/restart).
	GatewayRestartPath = "/ui/gateway/restart"
)

// GatewayControl reports on and restarts the gateway serving the page.
type GatewayControl interface {
	Gateway() (GatewayInfo, error)
	// Restart hands the restart to a separate process and returns at
	// once: that process stops this one, so the caller must answer
	// before then. It fails with *RestartUnavailableError when the
	// gateway cannot be restarted from here.
	Restart() (GatewayRestart, error)
}

// GatewayInfo is the body of GET /ui/gateway.
type GatewayInfo struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at,omitempty"`
	// LaunchMode is background, foreground or unknown; Lifetime is
	// explicit or on_demand.
	LaunchMode string `json:"launch_mode"`
	Lifetime   string `json:"lifetime"`
	// RestartAvailable is true for a background gateway recorded as
	// this process; otherwise RestartUnavailableReason says what to do.
	RestartAvailable         bool   `json:"restart_available"`
	RestartUnavailableReason string `json:"restart_unavailable_reason,omitempty"`
	// Restarting reports that a restart was already requested.
	Restarting bool `json:"restarting"`
}

// GatewayRestart is the body of an accepted POST /ui/gateway/restart.
type GatewayRestart struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// RestartUnavailableError is GatewayControl.Restart's error when the
// gateway cannot be restarted from the WebUI; it is answered with 409.
type RestartUnavailableError struct{ Reason string }

func (e *RestartUnavailableError) Error() string { return "restart unavailable: " + e.Reason }

// serveGateway answers GET /ui/gateway.
func (c *Channel) serveGateway(w http.ResponseWriter, _ *http.Request) {
	info, err := c.gateway.Gateway()
	if err != nil {
		c.log.Warn("websocket: gateway status", "error", err)
		http.Error(w, "cannot read the gateway status", http.StatusInternalServerError)
		return
	}
	writeJSON(w, info)
}

// restartGateway answers POST /ui/gateway/restart with 202 once the
// restart is handed off; the gateway goes away shortly after.
func (c *Channel) restartGateway(w http.ResponseWriter, _ *http.Request) {
	res, err := c.gateway.Restart()
	var unavailable *RestartUnavailableError
	switch {
	case errors.As(err, &unavailable):
		writeJSONStatus(w, http.StatusConflict, map[string]any{"ok": false, "error": unavailable.Reason})
		return
	case err != nil:
		c.log.Warn("websocket: restart gateway", "error", err)
		http.Error(w, "cannot restart the gateway", http.StatusInternalServerError)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, res)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
