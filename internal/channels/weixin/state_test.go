package weixin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChooseAccount(t *testing.T) {
	const cfgTok, savedTok = "cfg-token", "saved-token"
	cfg := Config{BaseURL: DefaultBaseURL}
	withTok := cfg
	withTok.Token = cfgTok
	saved := accountState{Token: savedTok, BaseURL: "https://assigned", GetUpdatesBuf: "cur",
		ContextTokens: map[string]string{"u": "ctx"}}
	replaced := saved
	replaced.ReplacedConfigTokenSHA256 = tokenFingerprint(cfgTok)
	sameTok := saved
	sameTok.Token = cfgTok

	for _, tc := range []struct {
		name          string
		cfg           Config
		st            accountState
		ok, fromState bool
		token, base   string
		cursor        string
	}{
		{"nothing", cfg, accountState{}, false, true, "", DefaultBaseURL, ""},
		{"saved login", cfg, saved, true, true, savedTok, "https://assigned", "cur"},
		{"configured token wins", withTok, saved, true, false, cfgTok, DefaultBaseURL, ""},
		{"login replaced the configured token", withTok, replaced, true, true, savedTok, "https://assigned", "cur"},
		{"configured token with its own state", withTok, sameTok, true, false, cfgTok, "https://assigned", "cur"},
		{"configured token only", withTok, accountState{}, true, false, cfgTok, DefaultBaseURL, ""},
	} {
		a, ok := chooseAccount(tc.cfg, tc.st)
		if ok != tc.ok || (ok && (a.token != tc.token || a.fromState != tc.fromState || a.baseURL != tc.base || a.cursor != tc.cursor)) {
			t.Errorf("%s: %+v, %t", tc.name, a, ok)
		}
	}
}

func TestStateFile(t *testing.T) {
	dir := t.TempDir()
	sf := stateFile{filepath.Join(dir, "weixin", StateFileName)}
	if st, err := sf.load(); err != nil || st.Token != "" {
		t.Fatalf("missing file: %+v, %v", st, err)
	}
	if ok, err := HasSavedLogin(sf.path); ok || err != nil {
		t.Errorf("HasSavedLogin = %t, %v", ok, err)
	}
	err := sf.update(func(st *accountState) bool {
		st.Token, st.ContextTokens = " tok ", map[string]string{"u": "c"}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(sf.path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", fi.Mode(), err)
	}
	if ok, err := HasSavedLogin(sf.path); !ok || err != nil {
		t.Errorf("HasSavedLogin = %t, %v", ok, err)
	}
	// nanobot's layout, so either can read the other's file.
	var raw map[string]any
	data, _ := os.ReadFile(sf.path)
	json.Unmarshal(data, &raw)
	for _, k := range []string{"token", "get_updates_buf", "context_tokens", "base_url"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("account.json lacks %q: %s", k, data)
		}
	}
	if st, _ := sf.load(); st.Token != "tok" {
		t.Errorf("token = %q", st.Token)
	}
	// A corrupt file is an error and is not overwritten.
	os.WriteFile(sf.path, []byte(`{"token": "SECRET-x`), 0o600)
	if _, err := sf.load(); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("corrupt load = %v", err)
	}
	if err := sf.update(func(*accountState) bool { return true }); err == nil {
		t.Error("update over a corrupt file succeeded")
	}
}

func TestSaveStateKeepsANewerLogin(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.saveLogin(t, "newer-SECRET") // a login while the channel runs
	h.ch.saveState()
	if st, _ := h.state.load(); st.Token != "newer-SECRET" {
		t.Errorf("token = %q", st.Token)
	}

	// A configured token is authoritative over an older saved token.
	h2 := newHarness(t, func(c *Config) { c.Token = "cfg-SECRET" })
	h2.start(t)
	h2.ch.saveState()
	if st, _ := h2.state.load(); st.Token != "cfg-SECRET" {
		t.Errorf("token = %q", st.Token)
	}
}

func TestParseConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	cfg, err := ParseConfig(json.RawMessage(`{"enabled": true}`), nil)
	if err != nil || cfg.BaseURL != DefaultBaseURL || cfg.PollTimeout != DefaultPollTimeout || cfg.Token != "" {
		t.Errorf("defaults = %+v, %v", cfg, err)
	}
	if got := cfg.StatePath("/x/state/weixin"); got != "/x/state/weixin/account.json" {
		t.Errorf("StatePath = %q", got)
	}
	if got := DefaultStateDir("/home/u/.nanobot-go/config.json"); got != "/home/u/.nanobot-go/state/weixin" {
		t.Errorf("DefaultStateDir = %q", got)
	}

	cfg, err = ParseConfig(json.RawMessage(`{"enabled": true, "token": "a", "baseUrl": "https://h/", "routeTag": 1001,
		"stateDir": "~/wx", "pollTimeout": 20, "allowFrom": ["u@im.wechat"]}`), env(map[string]string{EnvToken: "from-env"}))
	if err != nil || cfg.Token != "from-env" || cfg.BaseURL != "https://h" || cfg.RouteTag != "1001" ||
		cfg.StateDir != filepath.Join(home, "wx") || cfg.PollTimeout != 20 || cfg.AllowFrom[0] != "u@im.wechat" {
		t.Errorf("cfg = %+v, %v", cfg, err)
	}

	for name, raw := range map[string]string{
		"unknown key":       `{"cdnBaseUrl": "https://cdn"}`,
		"bad base URL":      `{"baseUrl": "ftp://h"}`,
		"base URL query":    `{"baseUrl": "https://h/?a=1"}`,
		"negative timeout":  `{"pollTimeout": -1}`,
		"token with spaces": `{"token": "a SECRET"}`,
		"bad routeTag":      `{"routeTag": true}`,
	} {
		_, err := ParseConfig(json.RawMessage(raw), nil)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSplitMessage(t *testing.T) {
	if got := splitMessage("  ", maxMessageLen); got != nil {
		t.Errorf("blank = %q", got)
	}
	if got := splitMessage("short", maxMessageLen); len(got) != 1 || got[0] != "short" {
		t.Errorf("short = %q", got)
	}
	// Fences stay balanced in every chunk, and chunks stay within limit.
	code := "```go\n" + strings.Repeat("fmt.Println(1)\n", 30) + "```"
	text := "intro\n\n" + code + "\n\nouter"
	chunks := splitMessage(text, 120)
	if len(chunks) < 3 {
		t.Fatalf("chunks = %q", chunks)
	}
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > 120 {
			t.Errorf("chunk %d has %d characters", i, n)
		}
		if strings.Count(c, "```")%2 != 0 {
			t.Errorf("chunk %d leaves a fence open:\n%s", i, c)
		}
	}
	// CJK text is split by characters, at sentence ends.
	cjk := strings.Repeat("你好世界。", 500)
	for i, c := range splitMessage(cjk, maxMessageLen) {
		if n := utf8.RuneCountInString(c); n > maxMessageLen || !strings.HasSuffix(c, "。") {
			t.Errorf("CJK chunk %d: %d characters", i, n)
		}
	}
}

func TestSanitizeMarkdown(t *testing.T) {
	for in, want := range map[string]string{
		"a <b> c":                   "a ＜b＞ c",
		"keep `x<y>` code":          "keep `x<y>` code",
		"```\n<html>\n```":          "```\n<html>\n```",
		"~~gone~~ ![p](u.png) text": "gone  text",
		"##### deep\n## shallow":    "deep\n## shallow",
		"":                          "",
	} {
		if got := sanitizeMarkdown(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
