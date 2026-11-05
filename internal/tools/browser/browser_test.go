package browser

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yedamao/wally/internal/config"
	"github.com/yedamao/wally/internal/security"
)

type fakeBackend struct{ calls []string }

func (f *fakeBackend) Navigate(_ context.Context, v string) error {
	f.calls = append(f.calls, "navigate "+v)
	return nil
}
func (f *fakeBackend) Snapshot(context.Context, int) ([]Element, error) {
	return []Element{{Ref: 1, Tag: "button", Name: "Submit"}}, nil
}
func (f *fakeBackend) Click(_ context.Context, v int) error {
	f.calls = append(f.calls, "click")
	return nil
}
func (f *fakeBackend) Type(_ context.Context, v int, s string, b bool) error {
	f.calls = append(f.calls, "type")
	return nil
}
func (f *fakeBackend) Select(_ context.Context, v int, s string) error {
	f.calls = append(f.calls, "select")
	return nil
}
func (f *fakeBackend) Scroll(_ context.Context, s string, v int) error {
	f.calls = append(f.calls, "scroll")
	return nil
}
func (f *fakeBackend) Key(_ context.Context, s string) error {
	f.calls = append(f.calls, "key")
	return nil
}
func (f *fakeBackend) Back(context.Context) error { f.calls = append(f.calls, "back"); return nil }
func (f *fakeBackend) ReadText(context.Context) (string, error) {
	f.calls = append(f.calls, "read_text")
	return "the number is 42", nil
}
func (f *fakeBackend) URL() string                              { return "https://example.com/page" }
func (f *fakeBackend) Screenshot(context.Context, string) error { return nil }

func testTool(f *fakeBackend, domains ...string) *Tool {
	c := config.Default().Tools.Browser
	c.AllowedDomains = domains
	t := New(Config{Browser: c, Policy: &security.Policy{}})
	t.newBackend = func() (backend, error) { return f, nil }
	return t
}
func raw(v string) json.RawMessage { return json.RawMessage(v) }

func TestDefaultsAndSchema(t *testing.T) {
	c := config.Default().Tools.Browser
	if c.Enable || !c.Headless || c.IncludeScreenshot || c.MaxElements != 200 || c.MaxSessions != 8 {
		t.Fatalf("defaults = %+v", c)
	}
	enum := testTool(&fakeBackend{}).Parameters()["properties"].(map[string]any)["action"].(map[string]any)["enum"].([]string)
	for _, a := range actions {
		if !contains(enum, a) {
			t.Errorf("schema missing %s", a)
		}
	}
}

func TestDispatch(t *testing.T) {
	tests := []struct {
		name, args, call string
		direct           bool
	}{
		{"navigate", `{"action":"navigate","url":"https://1.1.1.1"}`, "navigate", false},
		{"snapshot", `{"action":"snapshot"}`, "", false}, {"click", `{"action":"click","ref":1}`, "click", false},
		{"type", `{"action":"type","ref":1,"text":"Ada","submit":true}`, "type", false},
		{"select", `{"action":"select","ref":1,"value":"one"}`, "select", false},
		{"scroll", `{"action":"scroll","scroll_direction":"down","scroll_amount":4}`, "scroll", false},
		{"key", `{"action":"key","text":"Enter"}`, "key", false}, {"back", `{"action":"back"}`, "back", false},
		{"read", `{"action":"read_text"}`, "read_text", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeBackend{}
			r := testTool(f).Execute(context.Background(), raw(tt.args))
			if r.IsError {
				t.Fatal(r.Content)
			}
			if tt.call != "" && (len(f.calls) == 0 || !strings.HasPrefix(f.calls[0], tt.call)) {
				t.Fatalf("calls %v", f.calls)
			}
			if tt.direct && strings.Contains(r.Content, "Interactive elements") {
				t.Fatal("read_text included snapshot")
			}
			if !tt.direct && !strings.Contains(r.Content, `[1] button "Submit"`) {
				t.Fatalf("result %q", r.Content)
			}
		})
	}
}

func TestErrorsAndPolicy(t *testing.T) {
	tests := []struct{ name, args, want string }{
		{"unknown", `{"action":"teleport"}`, "unknown action"}, {"missing ref", `{"action":"click"}`, "requires an element 'ref'"},
		{"domain", `{"action":"navigate","url":"https://evil.test"}`, "allowedDomains"},
		{"private", `{"action":"navigate","url":"http://127.0.0.1"}`, "blocked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			domains := []string(nil)
			if tt.name == "domain" {
				domains = []string{"example.com"}
			}
			r := testTool(&fakeBackend{}, domains...).Execute(context.Background(), raw(tt.args))
			if !r.IsError || !strings.Contains(r.Content, tt.want) {
				t.Fatalf("result %+v", r)
			}
		})
	}
}

func TestAllowedDomainSubdomains(t *testing.T) {
	if !domainAllowed("app.example.com", []string{"example.com"}) || domainAllowed("badexample.com", []string{"example.com"}) {
		t.Fatal("domain matching")
	}
}
