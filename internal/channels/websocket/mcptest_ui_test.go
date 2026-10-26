package websocket

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// fakeMCPTester knows only "fs", which connects.
type fakeMCPTester struct{ calls int }

func (f *fakeMCPTester) TestMCPServer(_ context.Context, name string) (MCPTestResult, error) {
	if name != "fs" {
		return MCPTestResult{}, ErrMCPNotFound
	}
	f.calls++
	return MCPTestResult{OK: true, Status: MCPTestOK, Message: "fs connected with 1 tool.", Tools: []string{"mcp_fs_read"}}, nil
}

func TestMCPTestRoute(t *testing.T) {
	const path = MCPPath + "/fs/test"
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1"})
	if code, _ := skillsRequest(t, h, http.MethodPost, path, "{}"); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("POST without tester = %d", code)
	}

	tester := &fakeMCPTester{}
	h = newHarnessWith(t, false, Options{Host: "127.0.0.1", MCPTester: tester})
	for _, body := range []string{"{}", ""} {
		code, data := skillsRequest(t, h, http.MethodPost, path, body)
		if code != http.StatusOK || !strings.Contains(string(data), `"ok":true`) || !strings.Contains(string(data), `"status":"ok"`) ||
			!strings.Contains(string(data), `"tools":["mcp_fs_read"]`) {
			t.Errorf("POST %q = %d %s", body, code, data)
		}
	}
	if tester.calls != 2 {
		t.Errorf("calls = %d", tester.calls)
	}
	if code, _ := skillsRequest(t, h, http.MethodPost, MCPPath+"/nope/test", "{}"); code != http.StatusNotFound {
		t.Errorf("unknown = %d", code)
	}
	if code, _ := skillsRequest(t, h, http.MethodGet, path, ""); code == http.StatusOK {
		t.Errorf("GET = %d", code)
	}

	// Cross-origin and non-local unsigned requests are refused.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST = %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(`{}`))
	req.Host = "evil.example"
	if resp, err = h.srv.Client().Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign host POST = %d", resp.StatusCode)
	}
	if tester.calls != 2 {
		t.Errorf("refused requests reached the tester: calls = %d", tester.calls)
	}
}
