package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	playwright "github.com/mxschmitt/playwright-go"
)

const snapshotJS = `(max) => {
 const SEL='a,button,input,textarea,select,[role=button],[role=link],[role=checkbox],[role=radio],[role=tab],[role=menuitem],[role=switch],[onclick],[contenteditable=""],[contenteditable=true]';
 const out=[]; let ref=0;
 for (const el of document.querySelectorAll(SEL)) { const r=el.getBoundingClientRect(),s=getComputedStyle(el); if(r.width<=0||r.height<=0||s.visibility==='hidden'||s.display==='none'||s.opacity==='0')continue; ref++; el.setAttribute('data-nanobot-ref',String(ref)); let name=(el.getAttribute('aria-label')||el.innerText||el.value||el.getAttribute('placeholder')||el.getAttribute('name')||el.getAttribute('title')||'').replace(/\s+/g,' ').trim().slice(0,120); out.push({ref,tag:el.tagName.toLowerCase(),role:el.getAttribute('role')||'',type:el.getAttribute('type')||'',name,href:el.getAttribute('href')||''}); if(out.length>=max)break; }
 return out;
}`

type playwrightBackend struct {
	cfg     Config
	pw      *playwright.Playwright
	browser playwright.Browser
	context playwright.BrowserContext
	page    playwright.Page
}

func newPlaywrightBackend(cfg Config) (backend, error) {
	if err := validateURL(context.Background(), cfg.Browser.StartURL, cfg.Browser.AllowedDomains, cfg.Policy, true); err != nil {
		return nil, fmt.Errorf("startUrl: %w", err)
	}
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	b, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{Headless: playwright.Bool(cfg.Browser.Headless)})
	if err != nil {
		_ = pw.Stop()
		return nil, err
	}
	p := &playwrightBackend{cfg: cfg, pw: pw, browser: b}
	bc, err := b.NewContext(playwright.BrowserNewContextOptions{Viewport: &playwright.Size{Width: cfg.Browser.Width, Height: cfg.Browser.Height}, DeviceScaleFactor: playwright.Float(1), ServiceWorkers: playwright.ServiceWorkerPolicyBlock})
	if err != nil {
		_ = b.Close()
		_ = pw.Stop()
		return nil, err
	}
	p.context = bc
	if err = bc.Route("**/*", p.route); err != nil {
		return nil, err
	}
	if err = bc.RouteWebSocket("**/*", p.routeWebSocket); err != nil {
		return nil, err
	}
	p.page, err = bc.NewPage()
	if err != nil {
		return nil, err
	}
	if cfg.Browser.StartURL != "about:blank" {
		if err = p.Navigate(context.Background(), cfg.Browser.StartURL); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *playwrightBackend) route(r playwright.Route) {
	req := r.Request()
	err := validateURL(context.Background(), req.URL(), p.cfg.Browser.AllowedDomains, p.cfg.Policy, req.IsNavigationRequest())
	if err != nil {
		_ = r.Abort("blockedbyclient")
		return
	}
	_ = r.Continue()
}
func (p *playwrightBackend) routeWebSocket(ws playwright.WebSocketRoute) {
	if validateURL(context.Background(), ws.URL(), nil, p.cfg.Policy, false) != nil {
		ws.Close()
		return
	}
	_, _ = ws.ConnectToServer()
}
func (p *playwrightBackend) Navigate(ctx context.Context, u string) error {
	_, err := p.page.Goto(u)
	return err
}
func (p *playwrightBackend) Snapshot(ctx context.Context, max int) ([]Element, error) {
	v, err := p.page.Evaluate(snapshotJS, max)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var es []Element
	err = json.Unmarshal(data, &es)
	return es, err
}
func selector(ref int) string { return fmt.Sprintf(`[data-nanobot-ref="%d"]`, ref) }
func (p *playwrightBackend) Click(ctx context.Context, ref int) error {
	return p.page.Click(selector(ref), playwright.PageClickOptions{Timeout: playwright.Float(5000)})
}
func (p *playwrightBackend) Type(ctx context.Context, ref int, text string, submit bool) error {
	s := selector(ref)
	if err := p.page.Fill(s, text, playwright.PageFillOptions{Timeout: playwright.Float(5000)}); err != nil {
		return err
	}
	if submit {
		return p.page.Press(s, "Enter")
	}
	return nil
}
func (p *playwrightBackend) Select(ctx context.Context, ref int, value string) error {
	_, err := p.page.SelectOption(selector(ref), playwright.SelectOptionValues{Values: &[]string{value}}, playwright.PageSelectOptionOptions{Timeout: playwright.Float(3000)})
	if err == nil {
		return nil
	}
	_, err = p.page.SelectOption(selector(ref), playwright.SelectOptionValues{Labels: &[]string{value}}, playwright.PageSelectOptionOptions{Timeout: playwright.Float(3000)})
	return err
}
func (p *playwrightBackend) Scroll(ctx context.Context, direction string, amount int) error {
	px := max(1, amount) * 100
	dx, dy := 0, 0
	switch direction {
	case "right":
		dx = px
	case "left":
		dx = -px
	case "down":
		dy = px
	case "up":
		dy = -px
	}
	_, err := p.page.Evaluate(`([x,y]) => window.scrollBy(x,y)`, []int{dx, dy})
	return err
}
func (p *playwrightBackend) Key(ctx context.Context, key string) error {
	return p.page.Keyboard().Press(playwrightKey(key))
}
func (p *playwrightBackend) Back(ctx context.Context) error { _, err := p.page.GoBack(); return err }
func (p *playwrightBackend) ReadText(ctx context.Context) (string, error) {
	v, err := p.page.Evaluate(`() => document.body ? document.body.innerText : ''`)
	if err != nil {
		return "", err
	}
	s, _ := v.(string)
	return truncate(s, 4000), nil
}
func (p *playwrightBackend) URL() string { return p.page.URL() }
func (p *playwrightBackend) Screenshot(ctx context.Context, path string) error {
	_, err := p.page.Screenshot(playwright.PageScreenshotOptions{Path: playwright.String(path)})
	return err
}
func playwrightKey(combo string) string {
	mods := map[string]string{"ctrl": "Control", "control": "Control", "alt": "Alt", "option": "Alt", "shift": "Shift", "cmd": "Meta", "meta": "Meta", "super": "Meta", "win": "Meta", "return": "Enter", "esc": "Escape", "space": "Space", "up": "ArrowUp", "down": "ArrowDown", "left": "ArrowLeft", "right": "ArrowRight", "pagedown": "PageDown", "page_down": "PageDown", "pageup": "PageUp", "page_up": "PageUp"}
	parts := strings.Split(combo, "+")
	for i, s := range parts {
		k := strings.ToLower(strings.TrimSpace(s))
		if v, ok := mods[k]; ok {
			parts[i] = v
		} else if len(s) > 1 {
			parts[i] = strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
		}
	}
	return strings.Join(parts, "+")
}
