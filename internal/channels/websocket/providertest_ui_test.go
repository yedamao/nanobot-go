package websocket

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// fakeTester is a ProviderTester knowing only anthropic, whose saved key
// is accepted.
type fakeTester struct{ last ProviderTest }

func (f *fakeTester) TestProvider(_ context.Context, name string, t ProviderTest) (ProviderTestResult, error) {
	if name != "anthropic" {
		return ProviderTestResult{}, ErrProviderNotFound
	}
	if t.APIKey != nil && *t.APIKey == "***" {
		return ProviderTestResult{}, fmt.Errorf("%w: apiKey is a redacted placeholder", ErrInvalidProvider)
	}
	f.last = t
	if t.APIKey != nil {
		return ProviderTestResult{Status: ProviderTestRejected, Message: "anthropic rejected the credential (HTTP 401)."}, nil
	}
	return ProviderTestResult{OK: true, Status: ProviderTestOK, Message: "anthropic accepted the credential."}, nil
}

func TestProviderTestRoute(t *testing.T) {
	const path = ProvidersPath + "/anthropic/test"
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1"})
	if code, _ := skillsRequest(t, h, http.MethodPost, path, "{}"); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("POST without tester = %d", code)
	}

	tester := &fakeTester{}
	h = newHarnessWith(t, false, Options{Host: "127.0.0.1", ProviderTester: tester})
	for _, body := range []string{"{}", ""} {
		code, data := skillsRequest(t, h, http.MethodPost, path, body)
		if code != http.StatusOK || !strings.Contains(string(data), `"ok":true`) || !strings.Contains(string(data), `"status":"ok"`) {
			t.Errorf("POST %q = %d %s", body, code, data)
		}
	}
	const draft = "sk-draft-route-key"
	code, data := skillsRequest(t, h, http.MethodPost, path, `{"apiKey":"`+draft+`","apiBase":""}`)
	if code != http.StatusOK || !strings.Contains(string(data), `"status":"rejected"`) || strings.Contains(string(data), draft) ||
		tester.last.APIKey == nil || tester.last.APIBase == nil || *tester.last.APIBase != "" {
		t.Errorf("draft = %d %s, test %+v", code, data, tester.last)
	}
	if code, _ := skillsRequest(t, h, http.MethodPost, path, `{"apiKey":"***"}`); code != http.StatusBadRequest {
		t.Errorf("redacted = %d", code)
	}
	for _, bad := range []string{`{"bogus":1}`, `[]`, `{`} {
		if code, _ := skillsRequest(t, h, http.MethodPost, path, bad); code != http.StatusBadRequest {
			t.Errorf("POST %q = %d", bad, code)
		}
	}
	if code, _ := skillsRequest(t, h, http.MethodPost, ProvidersPath+"/custom/test", "{}"); code != http.StatusNotFound {
		t.Errorf("unknown = %d", code)
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
}
