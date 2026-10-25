package websocket

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// fakeSettings is a SettingsStore over a Settings value.
type fakeSettings struct {
	saved   Settings
	patches []SettingsPatch
	err     error
}

func (f *fakeSettings) Settings() (SettingsView, error) {
	return SettingsView{Settings: f.saved}, nil
}

func (f *fakeSettings) UpdateSettings(p SettingsPatch) (SettingsView, error) {
	f.patches = append(f.patches, p)
	if f.err != nil {
		return SettingsView{}, f.err
	}
	if p.Model != nil {
		f.saved.Model = *p.Model
	}
	if p.Tools != nil && p.Tools.Exec != nil {
		f.saved.Tools.Exec = *p.Tools.Exec
	}
	return SettingsView{Settings: f.saved, RestartRequired: []string{"model"}}, nil
}

func TestParseSettingsPatch(t *testing.T) {
	p, err := ParseSettingsPatch([]byte(`{"model":"m","provider":"auto",` +
		`"tools":{"exec":false,"web_fetch":true,"exec_sandbox":"seatbelt"},` +
		`"channels":{"telegram":{"enabled":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if *p.Model != "m" || *p.Provider != "auto" || *p.Tools.Exec || !*p.Tools.WebFetch ||
		*p.Tools.ExecSandbox != "seatbelt" || !*p.Channels["telegram"].Enabled {
		t.Errorf("patch = %+v", p)
	}
	if p, err := ParseSettingsPatch([]byte(`{}`)); err != nil || p.Model != nil || p.Tools != nil || p.Channels != nil || p.FallbackModels != nil {
		t.Errorf("empty patch = %+v, %v", p, err)
	}
	p, err = ParseSettingsPatch([]byte(`{"fallbackModels":[{"provider":"openai","model":"gpt-x"},{"model":"m2"}]}`))
	if err != nil || p.FallbackModels == nil ||
		!slices.Equal(*p.FallbackModels, []FallbackModel{{Provider: "openai", Model: "gpt-x"}, {Model: "m2"}}) {
		t.Errorf("fallback patch = %+v, %v", p, err)
	}
	if p, err := ParseSettingsPatch([]byte(`{"fallbackModels":[]}`)); err != nil || p.FallbackModels == nil || len(*p.FallbackModels) != 0 {
		t.Errorf("clearing fallback patch = %+v, %v", p, err)
	}

	for _, body := range []string{
		``,
		`[]`,
		`"model"`,
		`{"model":"m"} {"model":"n"}`,
		`{"apiKey":"x"}`,
		`{"api_key":"x"}`,
		`{"providers":{"anthropic":{"apiKey":"x"}}}`,
		`{"agents":{"defaults":{"model":"m"}}}`,
		`{"Model":"m"}`,
		`{"model":null}`,
		`{"model":1}`,
		`{"workspace":"/tmp"}`,
		`{"tools":true}`,
		`{"tools":{"exec":"yes"}}`,
		`{"tools":{"exec":{"enable":true}}}`,
		`{"tools":{"mcpServers":{}}}`,
		`{"tools":{"ssrfWhitelist":[]}}`,
		`{"tools":{"exec_sandbox":null}}`,
		`{"channels":{"telegram":{"token":"x"}}}`,
		`{"channels":{"telegram":{"allowFrom":["1"]}}}`,
		`{"channels":{"telegram":{"enabled":true,"token":"x"}}}`,
		`{"channels":{"telegram":{"Enabled":true}}}`,
		`{"channels":{"telegram":true}}`,
		`{"channels":{"token":{"enabled":true}}}`,
		`{"channels":{"telegram":{"headers":{}}}}`,
		`{"api":{"apiKey":"x"}}`,
		`{"env":{}}`,
		`{"secret":"x"}`,
		`{"password":"x"}`,
		`{"fallbackModels":null}`,
		`{"fallbackModels":{}}`,
		`{"fallbackModels":"m"}`,
		`{"fallbackModels":["m"]}`,
		`{"fallbackModels":[null]}`,
		`{"fallbackModels":[[]]}`,
		`{"fallbackModels":[{"model":1}]}`,
		`{"fallbackModels":[{"model":null}]}`,
		`{"fallbackModels":[{"model":"m","apiKey":"x"}]}`,
		`{"fallbackModels":[{"model":"m","apiBase":"https://x"}]}`,
		`{"fallbackModels":[{"Model":"m"}]}`,
	} {
		if p, err := ParseSettingsPatch([]byte(body)); err == nil {
			t.Errorf("ParseSettingsPatch(%s) = %+v, want an error", body, p)
		}
	}
	if _, err := ParseSettingsPatch([]byte(`{"channels":{"telegram":{"token":"sk-123"}}}`)); err == nil ||
		!strings.Contains(err.Error(), "channels.telegram.token cannot be changed here") || strings.Contains(err.Error(), "sk-123") {
		t.Errorf("secret key error = %v", err)
	}
}

func TestSettingsEndpoints(t *testing.T) {
	store := &fakeSettings{saved: Settings{Model: "m", Provider: "auto", Tools: ToolSettings{Exec: true}}}
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1", Settings: store})
	do := func(method, host, origin, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, h.srv.URL+SettingsPath, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
			req.Header.Set("Sec-Fetch-Site", "cross-site")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}

	code, body := do(http.MethodGet, "", "", "")
	want := `{"settings":{"model":"m","provider":"auto","fallbackModels":[],"tools":{"exec":true,"web_fetch":false,"exec_sandbox":""},"channels":{}},` +
		`"restart_required":[],"env_overrides":[],"choices":{"provider":[],"exec_sandbox":[]}}` + "\n"
	if code != http.StatusOK || body != want {
		t.Errorf("GET = %d\n%s\nwant\n%s", code, body, want)
	}

	if code, _ := do(http.MethodGet, "evil.example", "", ""); code != http.StatusForbidden {
		t.Errorf("rebound GET = %d", code)
	}
	if code, _ := do(http.MethodPatch, "evil.example", "", `{"model":"x"}`); code != http.StatusForbidden {
		t.Errorf("rebound PATCH = %d", code)
	}
	if code, _ := do(http.MethodPatch, "", "http://evil.example", `{"model":"x"}`); code != http.StatusForbidden {
		t.Errorf("cross-origin PATCH = %d", code)
	}
	if code, _ := do(http.MethodPatch, "", "", `{"apiKey":"x"}`); code != http.StatusBadRequest {
		t.Errorf("secret PATCH = %d", code)
	}
	if code, _ := do(http.MethodPatch, "", "", `{"model":"`+strings.Repeat("x", maxSettingsBody)+`"}`); code != http.StatusBadRequest {
		t.Errorf("oversized PATCH = %d", code)
	}
	if len(store.patches) != 0 {
		t.Fatalf("refused requests reached the store: %+v", store.patches)
	}

	code, body = do(http.MethodPatch, "", "", `{"model":"n","tools":{"exec":false}}`)
	var v SettingsView
	if err := json.Unmarshal([]byte(body), &v); code != http.StatusOK || err != nil ||
		v.Settings.Model != "n" || v.Settings.Tools.Exec || len(v.RestartRequired) != 1 {
		t.Errorf("PATCH = %d %s", code, body)
	}

	store.err = fmt.Errorf("%w: tools.exec_sandbox %q is not one of: (none)", ErrInvalidSettings, "bwrap")
	if code, body := do(http.MethodPatch, "", "", `{"tools":{"exec_sandbox":"bwrap"}}`); code != http.StatusBadRequest ||
		strings.TrimSpace(body) != `tools.exec_sandbox "bwrap" is not one of: (none)` {
		t.Errorf("invalid PATCH = %d %q", code, body)
	}
	store.err = errors.New("save config: disk full at /private/path")
	if code, body := do(http.MethodPatch, "", "", `{"model":"x"}`); code != http.StatusInternalServerError || strings.Contains(body, "/private") {
		t.Errorf("failing PATCH = %d %q", code, body)
	}
}

func TestSettingsUnset(t *testing.T) {
	h := newHarnessWith(t, false, Options{Host: "127.0.0.1"})
	resp, err := http.Get(h.srv.URL + SettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET without a store = %d, want 404", resp.StatusCode)
	}
}
