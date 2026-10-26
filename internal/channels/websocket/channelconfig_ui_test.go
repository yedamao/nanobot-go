package websocket

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// fakeChannelConfig is an in-memory ChannelConfigStore.
type fakeChannelConfig struct {
	tokens    map[string]string
	lastPatch ChannelConfigPatch
	lastTest  ChannelConfigTest
	loggedIn  bool
}

func (f *fakeChannelConfig) view(name string) ChannelConfigView {
	return ChannelConfigView{Name: name, HasToken: f.tokens[name] != "", AllowFrom: []string{}, EnvOverrides: []string{}}
}

func (f *fakeChannelConfig) ChannelConfigs() (ChannelConfigList, error) {
	return ChannelConfigList{Channels: []ChannelConfigView{f.view("telegram")}}, nil
}

func (f *fakeChannelConfig) UpdateChannelConfig(name string, p ChannelConfigPatch) (ChannelConfigView, error) {
	if name != "telegram" {
		return ChannelConfigView{}, ErrChannelNotConfigurable
	}
	if p.Token != nil && *p.Token == "***" {
		return ChannelConfigView{}, fmt.Errorf("%w: token is a redacted placeholder", ErrInvalidChannelConfig)
	}
	f.lastPatch = p
	switch {
	case p.ClearToken:
		f.tokens[name] = ""
	case p.Token != nil:
		f.tokens[name] = *p.Token
	}
	return f.view(name), nil
}

func (f *fakeChannelConfig) TestChannelConfig(_ context.Context, name string, t ChannelConfigTest) (ChannelTestResult, error) {
	if name != "telegram" {
		return ChannelTestResult{}, ErrChannelNotConfigurable
	}
	f.lastTest = t
	return ChannelTestResult{OK: true, Status: ChannelTestOK, Message: "fine"}, nil
}

func (f *fakeChannelConfig) ClearChannelLogin(name string) (ChannelConfigView, error) {
	if name != "weixin" {
		return ChannelConfigView{}, ErrChannelNotConfigurable
	}
	f.loggedIn = false
	v := f.view(name)
	v.HasLoginState = &f.loggedIn
	v.Login = &WeixinLoginView{StatePath: "/state/weixin/account.json"}
	v.RestartRequired = true
	return v, nil
}

func TestChannelConfigRoutes(t *testing.T) {
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1"})
	if code, _ := skillsRequest(t, h, http.MethodGet, ChannelConfigPath, ""); code != http.StatusNotFound {
		t.Errorf("GET without store = %d", code)
	}

	const tok = "123456:route-fixture-secret"
	store := &fakeChannelConfig{tokens: map[string]string{"telegram": tok}, loggedIn: true}
	h = newHarnessWith(t, false, Options{Host: "127.0.0.1", ChannelConfig: store})
	code, body := skillsRequest(t, h, http.MethodGet, ChannelConfigPath, "")
	if code != http.StatusOK || !strings.Contains(string(body), `"hasToken":true`) || strings.Contains(string(body), tok) {
		t.Errorf("GET = %d %s", code, body)
	}

	code, body = skillsRequest(t, h, http.MethodPatch, ChannelConfigPath+"/telegram", `{"clearToken":true}`)
	if code != http.StatusOK || !strings.Contains(string(body), `"hasToken":false`) {
		t.Errorf("clear = %d %s", code, body)
	}
	code, body = skillsRequest(t, h, http.MethodPatch, ChannelConfigPath+"/telegram", `{"token":"9:new-route-secret","enabled":true,"allowFrom":["*"]}`)
	if code != http.StatusOK || strings.Contains(string(body), "new-route-secret") || store.lastPatch.Enabled == nil || !*store.lastPatch.Enabled ||
		store.lastPatch.AllowFrom == nil || len(*store.lastPatch.AllowFrom) != 1 {
		t.Errorf("set = %d %s, patch %+v", code, body, store.lastPatch)
	}
	if code, _ := skillsRequest(t, h, http.MethodPatch, ChannelConfigPath+"/telegram", `{"token":"***"}`); code != http.StatusBadRequest {
		t.Errorf("redacted = %d", code)
	}
	for _, bad := range []string{`{}`, `{"enabled":true,"tokenIssueSecret":"x"}`, `[]`, ``} {
		if code, _ := skillsRequest(t, h, http.MethodPatch, ChannelConfigPath+"/telegram", bad); code != http.StatusBadRequest {
			t.Errorf("patch %q = %d", bad, code)
		}
	}
	if code, _ := skillsRequest(t, h, http.MethodPatch, ChannelConfigPath+"/websocket", `{"enabled":true}`); code != http.StatusNotFound {
		t.Errorf("websocket = %d", code)
	}

	code, body = skillsRequest(t, h, http.MethodPost, ChannelConfigPath+"/telegram/test", `{"token":"1:draft"}`)
	if code != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) || store.lastTest.Token == nil {
		t.Errorf("test = %d %s", code, body)
	}
	if code, _ := skillsRequest(t, h, http.MethodPost, ChannelConfigPath+"/telegram/test", ""); code != http.StatusOK || store.lastTest.Token != nil {
		t.Errorf("test without body = %d %+v", code, store.lastTest)
	}

	// Cross-origin changes and tests are refused.
	for _, r := range []struct{ method, path string }{
		{http.MethodPatch, ChannelConfigPath + "/telegram"},
		{http.MethodPost, ChannelConfigPath + "/telegram/test"},
		{http.MethodDelete, ChannelConfigPath + "/weixin/login"},
	} {
		req, _ := http.NewRequest(r.method, h.srv.URL+r.path, strings.NewReader(`{"clearToken":true}`))
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("cross-origin %s %s = %d", r.method, r.path, resp.StatusCode)
		}
	}
	if !store.loggedIn {
		t.Error("a cross-origin request cleared the login")
	}

	code, body = skillsRequest(t, h, http.MethodDelete, ChannelConfigPath+"/weixin/login", "")
	if code != http.StatusOK || !strings.Contains(string(body), `"hasLoginState":false`) || !strings.Contains(string(body), `"loggedIn":false`) ||
		!strings.Contains(string(body), `"restart_required":true`) || store.loggedIn {
		t.Errorf("clear login = %d %s", code, body)
	}
	for _, name := range []string{"telegram", "websocket"} {
		if code, _ := skillsRequest(t, h, http.MethodDelete, ChannelConfigPath+"/"+name+"/login", ""); code != http.StatusNotFound {
			t.Errorf("clear %s login = %d", name, code)
		}
	}
	// Non-local requests without sign-in are refused.
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+ChannelConfigPath, nil)
	req.Host = "evil.example"
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign host GET = %d", resp.StatusCode)
	}
	store.loggedIn = true
	req, _ = http.NewRequest(http.MethodDelete, h.srv.URL+ChannelConfigPath+"/weixin/login", nil)
	req.Host = "evil.example"
	resp, err = h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !store.loggedIn {
		t.Errorf("foreign host DELETE login = %d", resp.StatusCode)
	}
}
