// Package browser provides DOM browser automation through Playwright.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
)

var actions = []string{"navigate", "snapshot", "click", "type", "select", "scroll", "key", "back", "read_text"}

// Element is one interactive DOM element in a snapshot.
type Element struct {
	Ref  int    `json:"ref"`
	Tag  string `json:"tag"`
	Role string `json:"role"`
	Type string `json:"type"`
	Name string `json:"name"`
	Href string `json:"href"`
}

type backend interface {
	Navigate(context.Context, string) error
	Snapshot(context.Context, int) ([]Element, error)
	Click(context.Context, int) error
	Type(context.Context, int, string, bool) error
	Select(context.Context, int, string) error
	Scroll(context.Context, string, int) error
	Key(context.Context, string) error
	Back(context.Context) error
	ReadText(context.Context) (string, error)
	URL() string
	Screenshot(context.Context, string) error
}

// Config supplies runtime-only dependencies in addition to tools.browser.
type Config struct {
	Browser   config.BrowserToolConfig
	Policy    *security.Policy
	Workspace string
}

// Tool controls a serialized browser context. The current tool context has no
// session identifier, so v1 deliberately uses one shared context.
type Tool struct {
	cfg        Config
	mu         sync.Mutex
	backend    backend
	newBackend func() (backend, error)
}

// New returns a lazily started browser tool.
func New(cfg Config) *Tool {
	t := &Tool{cfg: cfg}
	t.newBackend = func() (backend, error) { return newPlaywrightBackend(cfg) }
	return t
}

func (t *Tool) Name() string { return "browser" }
func (t *Tool) Description() string {
	return "Control a web browser using numbered DOM element refs. Call snapshot first, then click/type/select the latest refs; refs are reassigned after every action. Use read_text for page text."
}
func (t *Tool) ReadOnly() bool { return false }
func (t *Tool) Parameters() map[string]any {
	action := tools.String("Action to perform.")
	action["enum"] = actions
	direction := tools.String("Scroll direction.")
	direction["enum"] = []string{"up", "down", "left", "right"}
	ref := tools.Integer("Element ref from the latest snapshot (click/type/select).")
	ref["minimum"] = 1
	amount := tools.Integer("Scroll clicks (1-100).")
	amount["minimum"], amount["maximum"] = 1, 100
	return tools.Object(map[string]any{"action": action, "ref": ref, "text": tools.String("Text to type, or key/combo for key."), "url": tools.String("URL for navigate."), "value": tools.String("Option value or label for select."), "submit": tools.Boolean("Press Enter after typing."), "scroll_direction": direction, "scroll_amount": amount}, "action")
}

type arguments struct {
	Action    string  `json:"action"`
	Ref       *int    `json:"ref"`
	Text      *string `json:"text"`
	URL       string  `json:"url"`
	Value     *string `json:"value"`
	Submit    bool    `json:"submit"`
	Direction string  `json:"scroll_direction"`
	Amount    int     `json:"scroll_amount"`
}

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	a, err := tools.DecodeArgs[arguments](raw)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if !contains(actions, a.Action) {
		return tools.Errorf("unknown action %q; valid actions: %s", a.Action, strings.Join(actions, ", "))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.backend == nil {
		t.backend, err = t.newBackend()
		if err != nil {
			return tools.Errorf("could not initialize browser: %v (install Chromium with: go run github.com/mxschmitt/playwright-go/cmd/playwright@v0.6201.1 install chromium)", err)
		}
	}
	b := t.backend
	status := ""
	switch a.Action {
	case "navigate":
		if a.URL == "" {
			return tools.Errorf("action 'navigate' requires 'url'")
		}
		if err = validateURL(ctx, a.URL, t.cfg.Browser.AllowedDomains, t.cfg.Policy, true); err == nil {
			err = b.Navigate(ctx, a.URL)
		}
		status = "Navigated to " + a.URL
	case "snapshot":
		status = "Snapshot of the current page"
	case "click":
		if a.Ref == nil {
			return missingRef(a.Action)
		}
		err = b.Click(ctx, *a.Ref)
		status = fmt.Sprintf("Clicked element [%d]", *a.Ref)
	case "type":
		if a.Ref == nil {
			return missingRef(a.Action)
		}
		if a.Text == nil {
			return tools.Errorf("action 'type' requires 'text'")
		}
		err = b.Type(ctx, *a.Ref, *a.Text, a.Submit)
		status = fmt.Sprintf("Typed into [%d]", *a.Ref)
	case "select":
		if a.Ref == nil {
			return missingRef(a.Action)
		}
		if a.Value == nil {
			return tools.Errorf("action 'select' requires 'value'")
		}
		err = b.Select(ctx, *a.Ref, *a.Value)
		status = fmt.Sprintf("Selected %q in [%d]", *a.Value, *a.Ref)
	case "scroll":
		if a.Direction == "" {
			a.Direction = "down"
		}
		if !contains([]string{"up", "down", "left", "right"}, a.Direction) {
			return tools.Errorf("scroll_direction must be up/down/left/right")
		}
		if a.Amount == 0 {
			a.Amount = 3
		}
		err = b.Scroll(ctx, a.Direction, a.Amount)
		status = "Scrolled " + a.Direction
	case "key":
		if a.Text == nil || *a.Text == "" {
			return tools.Errorf("action 'key' requires 'text'")
		}
		err = b.Key(ctx, *a.Text)
		status = "Pressed " + *a.Text
	case "back":
		err = b.Back(ctx)
		status = "Navigated back"
	case "read_text":
		var text string
		text, err = b.ReadText(ctx)
		if err == nil {
			return tools.Text("Page text:\n" + text)
		}
	}
	if err != nil {
		return tools.Errorf("browser %s: %v", a.Action, err)
	}
	elements, snapErr := b.Snapshot(ctx, t.cfg.Browser.MaxElements)
	out := status + "\nCurrent page: " + b.URL() + "\n\n" + formatElements(elements)
	if snapErr != nil {
		out = status + "\nCurrent page: " + b.URL() + "\n\n(could not read page elements: " + snapErr.Error() + ")"
	}
	if t.cfg.Browser.IncludeScreenshot && t.cfg.Workspace != "" {
		dir := filepath.Join(t.cfg.Workspace, "media", "browser")
		_ = os.MkdirAll(dir, 0o700)
		path := filepath.Join(dir, fmt.Sprintf("browser-%d.png", time.Now().UnixNano()))
		if b.Screenshot(ctx, path) == nil {
			out += "\nScreenshot: " + path
		}
	}
	return tools.Text(out)
}

func missingRef(action string) tools.Result {
	return tools.Errorf("action %q requires an element 'ref' from the snapshot", action)
}
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
func formatElements(es []Element) string {
	if len(es) == 0 {
		return "Interactive elements: (none found — try scrolling or read_text)"
	}
	lines := []string{"Interactive elements (act with the [ref] number):"}
	for _, e := range es {
		label := e.Tag
		if e.Type != "" {
			label += "[" + e.Type + "]"
		}
		line := fmt.Sprintf("[%d] %s", e.Ref, label)
		if strings.TrimSpace(e.Name) != "" {
			line += fmt.Sprintf(" %q", e.Name)
		}
		if e.Tag == "a" && e.Href != "" {
			line += " -> " + truncate(e.Href, 60)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func validateURL(ctx context.Context, raw string, allowed []string, policy *security.Policy, navigation bool) error {
	if raw == "about:blank" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if !navigation && (u.Scheme == "data" || u.Scheme == "blob") {
		return nil
	}
	if navigation && len(allowed) > 0 && !domainAllowed(u.Hostname(), allowed) {
		return fmt.Errorf("host %q is not in allowedDomains", u.Hostname())
	}
	if u.Scheme == "ws" {
		u.Scheme = "http"
	}
	if u.Scheme == "wss" {
		u.Scheme = "https"
	}
	if _, err := policy.CheckTarget(ctx, u); err != nil {
		return fmt.Errorf("URL is blocked: %w", err)
	}
	return nil
}
func domainAllowed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, d := range allowed {
		d = strings.ToLower(strings.Trim(strings.TrimSpace(d), "."))
		if d != "" && (host == d || strings.HasSuffix(host, "."+d)) {
			return true
		}
	}
	return false
}
