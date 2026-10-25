package websocket

import (
	"net/http"
	"strings"
	"testing"
)

// fakeGateway is a GatewayControl whose restart only counts.
type fakeGateway struct {
	info     GatewayInfo
	restarts int
}

func (f *fakeGateway) Gateway() (GatewayInfo, error) { return f.info, nil }

func (f *fakeGateway) Restart() (GatewayRestart, error) {
	if !f.info.RestartAvailable {
		return GatewayRestart{}, &RestartUnavailableError{Reason: f.info.RestartUnavailableReason}
	}
	f.restarts++
	return GatewayRestart{OK: true, Message: "restarting"}, nil
}

func TestGatewayRoutes(t *testing.T) {
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1"})
	if code, _ := skillsRequest(t, h, http.MethodGet, GatewayPath, ""); code != http.StatusNotFound {
		t.Errorf("GET without control = %d", code)
	}

	fg := &fakeGateway{info: GatewayInfo{LaunchMode: "foreground", Lifetime: "explicit", RestartUnavailableReason: "restart it in its terminal"}}
	h = newHarnessWith(t, false, Options{Host: "127.0.0.1", Gateway: fg})
	code, body := skillsRequest(t, h, http.MethodGet, GatewayPath, "")
	if code != http.StatusOK || !strings.Contains(string(body), `"restart_available":false`) || !strings.Contains(string(body), "in its terminal") {
		t.Errorf("GET foreground = %d %s", code, body)
	}
	code, body = skillsRequest(t, h, http.MethodPost, GatewayRestartPath, "")
	if code != http.StatusConflict || !strings.Contains(string(body), "in its terminal") || fg.restarts != 0 {
		t.Errorf("POST foreground = %d %s, restarts %d", code, body, fg.restarts)
	}

	bg := &fakeGateway{info: GatewayInfo{LaunchMode: "background", Lifetime: "on_demand", RestartAvailable: true}}
	h = newHarnessWith(t, false, Options{Host: "127.0.0.1", Gateway: bg})
	code, body = skillsRequest(t, h, http.MethodPost, GatewayRestartPath, "")
	if code != http.StatusAccepted || !strings.Contains(string(body), `"ok":true`) || bg.restarts != 1 {
		t.Errorf("POST background = %d %s, restarts %d", code, body, bg.restarts)
	}

	// Cross-origin restarts are refused.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+GatewayRestartPath, nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || bg.restarts != 1 {
		t.Errorf("cross-origin POST = %d, restarts %d", resp.StatusCode, bg.restarts)
	}
	// Non-local requests without sign-in are refused.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := GatewayPath
		if method == http.MethodPost {
			path = GatewayRestartPath
		}
		req, _ = http.NewRequest(method, h.srv.URL+path, nil)
		req.Host = "evil.example"
		if resp, err = h.srv.Client().Do(req); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("foreign host %s = %d", method, resp.StatusCode)
		}
	}
	if bg.restarts != 1 {
		t.Errorf("restarts = %d", bg.restarts)
	}
}
