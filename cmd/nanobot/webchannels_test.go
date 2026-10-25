package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/channels/weixin"
	"github.com/yedamao/nanobot-go/internal/config"
)

// Fake Telegram tokens: the right shape, never real.
const (
	tgFixtureToken = "123456:AAfixtureSecretValue_abcd"
	tgOtherToken   = "123456:AAotherSecretValue_wxyz"
)

// runningChannels returns a channel config store for path whose running
// sections are the file's.
func runningChannels(t *testing.T, path string, env map[string]string, live ...string) *webChannels {
	t.Helper()
	s := runningSettings(t, path, env)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string { return env[k] }
	return newWebChannels(s, cfg.Channels, func() []string { return live }, getenv, filepath.Join(filepath.Dir(path), "state", "weixin"))
}

func channelByName(t *testing.T, list websocket.ChannelConfigList, name string) websocket.ChannelConfigView {
	t.Helper()
	for _, v := range list.Channels {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no channel %q in %+v", name, list)
	return websocket.ChannelConfigView{}
}

// assertNoToken fails when v's JSON holds any of the tokens.
func assertNoToken(t *testing.T, v any, tokens ...string) {
	t.Helper()
	assertNoSecrets(t, v)
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range append(tokens, tgFixtureToken, tgOtherToken, "AAfixtureSecret", "AAotherSecret") {
		if strings.Contains(string(data), tok) {
			t.Errorf("JSON holds token %q: %s", tok, data)
		}
	}
}

// fileSection returns channels.<name> as saved.
func fileSection(t *testing.T, path, name string) map[string]any {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := cfg.Channels[name]
	if !ok {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// writeTelegramConfig writes a config with an enabled Telegram channel
// against apiBase and returns its path.
func writeTelegramConfig(t *testing.T, apiBase string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"agents": {"defaults": {"workspace": ` + jsonString(filepath.Join(dir, "workspace")) + `}},
  "channels": {"telegram": {"enabled": true, "token": "` + tgFixtureToken + `", "apiBase": "` + apiBase + `", "proxy": "http://user:proxypass@localhost:1"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWebChannelsListRedacts(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningChannels(t, path, map[string]string{"NANOBOT_GO_WEIXIN_TOKEN": envSecret}, "websocket")
	list, err := s.ChannelConfigs()
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, list)
	var names []string
	for _, v := range list.Channels {
		names = append(names, v.Name)
		if v.RestartRequired || v.Running {
			t.Errorf("%+v", v)
		}
	}
	if !slices.Equal(names, []string{"telegram", "weixin"}) {
		t.Errorf("names = %v", names)
	}
	tg := channelByName(t, list, "telegram")
	if !tg.Present || tg.Enabled || !tg.HasToken || tg.TokenHint != "sk-f••••gram" || !slices.Equal(tg.AllowFrom, []string{"12345"}) ||
		!tg.TestSupported || tg.DefaultBase == "" || len(tg.EnvOverrides) != 0 {
		t.Errorf("telegram = %+v", tg)
	}
	wx := channelByName(t, list, "weixin")
	if wx.Present || wx.HasToken || wx.HasLoginState == nil || *wx.HasLoginState || wx.TestSupported ||
		!slices.Equal(wx.EnvOverrides, []string{"token"}) {
		t.Errorf("weixin = %+v", wx)
	}
}

func TestWebChannelsCreateWeixin(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningChannels(t, path, nil)
	v, err := s.UpdateChannelConfig("weixin", websocket.ChannelConfigPatch{
		BaseURL: ptr(" https://ilink.example.test "), RouteTag: ptr("7"), AllowFrom: &[]string{" wx_user ", "", "wx_user"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Present || v.Enabled || v.BaseURL != "https://ilink.example.test" || v.RouteTag != "7" || !slices.Equal(v.AllowFrom, []string{"wx_user"}) || v.RestartRequired {
		t.Errorf("view = %+v", v)
	}
	sec := fileSection(t, path, "weixin")
	if sec["enabled"] != false || sec["baseUrl"] != "https://ilink.example.test" || sec["routeTag"] != "7" {
		t.Errorf("section = %v", sec)
	}
	// Other secrets survive.
	data, _ := os.ReadFile(path)
	for _, want := range []string{fileSecret + "-anthropic", fileSecret + "-telegram", fileSecret + "-mcp", fileSecret + "-custom"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("config lost %s", want)
		}
	}

	// Enabling it needs a restart; a logged-in state is reported.
	v, err = s.UpdateChannelConfig("weixin", websocket.ChannelConfigPatch{Enabled: ptr(true), BaseURL: ptr("")})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Enabled || !v.RestartRequired || v.BaseURL != "" {
		t.Errorf("enabled view = %+v", v)
	}
	if _, ok := fileSection(t, path, "weixin")["baseUrl"]; ok {
		t.Error("baseUrl not removed")
	}
	dir := filepath.Join(filepath.Dir(path), "state", "weixin")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, weixin.StateFileName), []byte(`{"token":"`+fileSecret+`-wxlogin"}`), 0o600)
	list, err := s.ChannelConfigs()
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, list)
	if wx := channelByName(t, list, "weixin"); wx.HasLoginState == nil || !*wx.HasLoginState || wx.HasToken {
		t.Errorf("weixin = %+v", wx)
	}

	// Telegram fields are refused.
	if _, err := s.UpdateChannelConfig("weixin", websocket.ChannelConfigPatch{GroupPolicy: ptr("open")}); !errors.Is(err, websocket.ErrInvalidChannelConfig) {
		t.Errorf("groupPolicy on weixin: %v", err)
	}
	if _, err := s.UpdateChannelConfig("websocket", websocket.ChannelConfigPatch{Enabled: ptr(true)}); !errors.Is(err, websocket.ErrChannelNotConfigurable) {
		t.Errorf("websocket: %v", err)
	}
}

func TestWebChannelsTelegramToken(t *testing.T) {
	path := writeSettingsConfig(t)
	s := runningChannels(t, path, nil)

	// Malformed, placeholder and hint tokens are refused without echo.
	for _, bad := range []string{"not-a-token-" + fileSecret, "***", "123456:••••abcd", "12 3:abc", ""} {
		_, err := s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{Token: ptr(bad)})
		if !errors.Is(err, websocket.ErrInvalidChannelConfig) {
			t.Errorf("token %q: err = %v", bad, err)
			continue
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("error echoes the token: %v", err)
		}
	}
	if fileSection(t, path, "telegram")["token"] != fileSecret+"-telegram" {
		t.Error("a refused token changed the file")
	}

	v, err := s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{
		Token: ptr(" " + tgFixtureToken + " "), Enabled: ptr(true), GroupPolicy: ptr("open"), ReplyToMessage: ptr(true), AllowFrom: &[]string{"*"}, PollTimeout: ptr(20),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, v)
	if !v.HasToken || v.TokenHint != "123456:••••abcd" || !v.Enabled || v.GroupPolicy != "open" || !v.ReplyToMessage || v.PollTimeout != 20 || !v.RestartRequired {
		t.Errorf("view = %+v", v)
	}
	sec := fileSection(t, path, "telegram")
	if sec["token"] != tgFixtureToken || sec["groupPolicy"] != "open" || sec["pollTimeout"] != float64(20) {
		t.Errorf("section = %v", sec)
	}

	// An enabled channel needs its token.
	if _, err := s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{ClearToken: true}); !errors.Is(err, websocket.ErrInvalidChannelConfig) ||
		!strings.Contains(err.Error(), "token is required") {
		t.Errorf("clear while enabled: %v", err)
	}
	// A disabled one may lack it.
	v, err = s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{ClearToken: true, Enabled: ptr(false), Token: ptr(tgOtherToken)})
	if err != nil {
		t.Fatal(err)
	}
	if v.HasToken || v.TokenHint != "" || v.Enabled || v.RestartRequired {
		t.Errorf("cleared view = %+v", v)
	}
	if _, ok := fileSection(t, path, "telegram")["token"]; ok {
		t.Error("token not removed")
	}

	if _, err := s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{GroupPolicy: ptr("all")}); !errors.Is(err, websocket.ErrInvalidChannelConfig) {
		t.Errorf("groupPolicy: %v", err)
	}
	if _, err := s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{APIBase: ptr("ftp://x")}); !errors.Is(err, websocket.ErrInvalidChannelConfig) {
		t.Errorf("apiBase: %v", err)
	}
	if _, err := s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{BaseURL: ptr("https://x.test")}); !errors.Is(err, websocket.ErrInvalidChannelConfig) {
		t.Errorf("baseUrl on telegram: %v", err)
	}

	// With the token in the environment, enabling needs none in the file.
	env := map[string]string{"NANOBOT_GO_TELEGRAM_TOKEN": tgOtherToken}
	s = runningChannels(t, path, env)
	v, err = s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{Enabled: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, v)
	if !slices.Equal(v.EnvOverrides, []string{"token"}) || v.HasToken {
		t.Errorf("env view = %+v", v)
	}
}

func TestWebChannelsRestartRequired(t *testing.T) {
	path := writeTelegramConfig(t, "https://api.telegram.example")
	s := runningChannels(t, path, nil, "telegram")
	list, err := s.ChannelConfigs()
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, list, "proxypass")
	tg := channelByName(t, list, "telegram")
	if tg.RestartRequired || !tg.Running || !tg.HasProxy {
		t.Errorf("telegram = %+v", tg)
	}
	v, err := s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{Token: ptr(tgOtherToken)})
	if err != nil {
		t.Fatal(err)
	}
	if !v.RestartRequired {
		t.Error("token change needs no restart")
	}
	// The proxy is kept.
	if sec := fileSection(t, path, "telegram"); sec["proxy"] != "http://user:proxypass@localhost:1" {
		t.Errorf("section = %v", sec)
	}
	if v, err = s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{Token: ptr(tgFixtureToken)}); err != nil || v.RestartRequired {
		t.Errorf("back = %+v, %v", v, err)
	}
	// Defaults written out make no difference.
	if v, err = s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{GroupPolicy: ptr("mention"), PollTimeout: ptr(30)}); err != nil || v.RestartRequired {
		t.Errorf("defaults = %+v, %v", v, err)
	}
}

func TestWebChannelsTestTelegram(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, tgFixtureToken) {
			w.Write([]byte(`{"ok":true,"result":{"id":123456,"is_bot":true,"username":"fixture_bot"}}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	}))
	defer srv.Close()

	path := writeTelegramConfig(t, srv.URL)
	s := runningChannels(t, path, nil)
	s.client = srv.Client()
	ctx := context.Background()

	res, err := s.TestChannelConfig(ctx, "telegram", websocket.ChannelConfigTest{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Status != websocket.ChannelTestOK || !strings.Contains(res.Message, "@fixture_bot") || gotPath != "/bot"+tgFixtureToken+"/getMe" {
		t.Errorf("saved = %+v (path %s)", res, gotPath)
	}
	res, err = s.TestChannelConfig(ctx, "telegram", websocket.ChannelConfigTest{Token: ptr(tgOtherToken)})
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, res)
	if res.OK || res.Status != websocket.ChannelTestRejected {
		t.Errorf("draft = %+v", res)
	}
	if _, err := s.TestChannelConfig(ctx, "telegram", websocket.ChannelConfigTest{Token: ptr("bad token")}); !errors.Is(err, websocket.ErrInvalidChannelConfig) {
		t.Errorf("bad draft: %v", err)
	}
	// The draft is never saved.
	if fileSection(t, path, "telegram")["token"] != tgFixtureToken {
		t.Error("draft saved")
	}

	// Unreachable: an error naming neither the token nor the proxy password.
	srv.Close()
	res, err = s.TestChannelConfig(ctx, "telegram", websocket.ChannelConfigTest{})
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, res, "proxypass")
	if res.OK || res.Status != websocket.ChannelTestError {
		t.Errorf("down = %+v", res)
	}

	// No token at all.
	path = writeSettingsConfig(t)
	s = runningChannels(t, path, nil)
	if _, err := s.UpdateChannelConfig("telegram", websocket.ChannelConfigPatch{ClearToken: true}); err != nil {
		t.Fatal(err)
	}
	if res, err = s.TestChannelConfig(ctx, "telegram", websocket.ChannelConfigTest{}); err != nil || res.Status != websocket.ChannelTestNotConfigured {
		t.Errorf("no token = %+v, %v", res, err)
	}
	if res, err = s.TestChannelConfig(ctx, "weixin", websocket.ChannelConfigTest{}); err != nil || res.OK || res.Status != websocket.ChannelTestUnsupported ||
		!strings.Contains(res.Message, "nanobot channels login weixin") {
		t.Errorf("weixin = %+v, %v", res, err)
	}
	if _, err = s.TestChannelConfig(ctx, "websocket", websocket.ChannelConfigTest{}); !errors.Is(err, websocket.ErrChannelNotConfigurable) {
		t.Errorf("websocket: %v", err)
	}
}
