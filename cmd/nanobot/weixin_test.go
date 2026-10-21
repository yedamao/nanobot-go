package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
)

const fakeWeixinToken = "wx-bot-token-SECRET"

// fakeILink serves the QR login (confirmed at once), getupdates (from a
// queue of responses, long polling otherwise) and records sendmessage.
type fakeILink struct {
	srv     *httptest.Server
	updates chan string // JSON arrays of messages
	sent    chan map[string]any
}

func newFakeILink(t *testing.T) *fakeILink {
	t.Helper()
	f := &fakeILink{updates: make(chan string, 8), sent: make(chan map[string]any, 8)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch endpoint {
		case "get_bot_qrcode":
			io.WriteString(w, `{"ret":0,"qrcode":"qr-1","qrcode_img_content":"https://liteapp.example/q/qr-1"}`)
			return
		case "get_qrcode_status":
			fmt.Fprintf(w, `{"status":"confirmed","bot_token":%q,"ilink_bot_id":"bot-1","baseurl":%q}`, fakeWeixinToken, "http://"+r.Host)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+fakeWeixinToken {
			io.WriteString(w, `{"ret":-14,"errmsg":"session timeout"}`)
			return
		}
		switch endpoint {
		case "getupdates":
			select {
			case msgs := <-f.updates:
				fmt.Fprintf(w, `{"ret":0,"msgs":%s,"get_updates_buf":"c"}`, msgs)
			case <-r.Context().Done():
			}
		case "sendmessage":
			f.sent <- body
			io.WriteString(w, `{"ret":0}`)
		default:
			io.WriteString(w, `{"ret":0}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeILink) dm(id, from, text string) {
	f.updates <- fmt.Sprintf(`[{"message_id":%q,"from_user_id":%q,"message_type":1,"context_token":"ctx-%s","item_list":[{"type":1,"text_item":{"text":%q}}]}]`,
		id, from, id, text)
}

func (f *fakeILink) waitSent(t *testing.T) (to, text string) {
	t.Helper()
	select {
	case body := <-f.sent:
		msg, _ := body["msg"].(map[string]any)
		to, _ = msg["to_user_id"].(string)
		items, _ := msg["item_list"].([]any)
		if len(items) > 0 {
			ti, _ := items[0].(map[string]any)["text_item"].(map[string]any)
			text, _ = ti["text"].(string)
		}
		return to, text
	case <-time.After(10 * time.Second):
		t.Fatal("nothing sent to WeChat")
		return "", ""
	}
}

func TestChannelsLoginWeixinAndGateway(t *testing.T) {
	e := newTestEnv(t)
	api := newFakeILink(t)
	e.writeGatewayConfig(t, func(c *config.Config) {
		delete(c.Channels, "fake")
		c.Channels["weixin"] = json.RawMessage(`{"enabled": true, "allowFrom": ["alice@im.wechat"], "pollTimeout": 1, "baseUrl": "` + api.srv.URL + `"}`)
	})
	statusOf := func() string {
		t.Helper()
		if code := e.run(t, nil, "status"); code != 0 {
			t.Fatalf("status exit = %d: %s", code, e.stderr)
		}
		return e.stdout.String()
	}
	if out := statusOf(); !strings.Contains(out, "weixin (enabled, not logged in (run 'nanobot channels login weixin'))") {
		t.Errorf("status before login:\n%s", out)
	}

	// The login saves the token next to the config file, never in it.
	if code := e.run(t, nil, "channels", "login", "weixin"); code != 0 {
		t.Fatalf("login exit = %d: %s", code, e.stderr)
	}
	statePath := filepath.Join(filepath.Dir(e.configPath), "state", "weixin", "account.json")
	fi, err := os.Stat(statePath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("account.json: %v, %v", fi, err)
	}
	if data, _ := os.ReadFile(e.configPath); strings.Contains(string(data), "SECRET") {
		t.Error("the token went into config.json")
	}
	if out := e.stdout.String(); !strings.Contains(out, "https://liteapp.example/q/qr-1") || !strings.Contains(out, "Logged in to WeChat as bot bot-1") {
		t.Errorf("login output:\n%s", out)
	}
	if out := statusOf(); !strings.Contains(out, "weixin (enabled, logged in)") {
		t.Errorf("status after login:\n%s", out)
	}
	if code := e.run(t, nil, "channels", "login", "weixin", "--config", e.configPath); code != 0 || !strings.Contains(e.stdout.String(), "already logged in") {
		t.Errorf("second login: exit %d\n%s", code, e.stdout)
	}

	g := startGateway(t, e, nil)
	if !strings.Contains(e.stdout.String(), "Channels: weixin") {
		t.Errorf("banner = %q", e.stdout)
	}
	api.dm("m1", "alice@im.wechat", "hello")
	if to, text := api.waitSent(t); to != "alice@im.wechat" || text != "echo: hello" {
		t.Errorf("reply to %s: %q", to, text)
	}

	// An unknown sender gets a pairing code; approving it lets them in.
	api.dm("m2", "bob@im.wechat", "let me in")
	to, text := api.waitSent(t)
	code := regexp.MustCompile(`[A-Z0-9]{4}-[A-Z0-9]{4}`).FindString(text)
	if to != "bob@im.wechat" || code == "" {
		t.Fatalf("pairing reply to %s: %q", to, text)
	}
	if code := e.run(t, nil, "pairing", "approve", code); code != 0 {
		t.Fatalf("pairing approve exit = %d: %s", code, e.stderr)
	}
	api.dm("m3", "bob@im.wechat", "thanks")
	if to, text := api.waitSent(t); to != "bob@im.wechat" || text != "echo: thanks" {
		t.Errorf("reply after approval to %s: %q", to, text)
	}

	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	if s, err := e.loadSession(t, "weixin:alice@im.wechat"); err != nil || len(s.Messages) != 2 {
		t.Errorf("session weixin:alice@im.wechat = %+v, %v", s, err)
	}
	for name, out := range map[string]string{"stdout": e.stdout.String(), "stderr": e.stderr.String()} {
		if strings.Contains(out, "SECRET") {
			t.Errorf("%s leaks the bot token:\n%s", name, out)
		}
	}
}

func TestGatewayWeixinNotLoggedIn(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, func(c *config.Config) {
		delete(c.Channels, "fake")
		c.Channels["weixin"] = json.RawMessage(`{"enabled": true}`)
	})
	g := startGateway(t, e, nil)
	if !strings.Contains(e.stdout.String(), "Channels: none running") || !strings.Contains(e.stderr.String(), "nanobot channels login weixin") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", e.stdout, e.stderr)
	}
	g.cancel()
	g.wait(t)
}

func TestChannelsCommandUsage(t *testing.T) {
	e := newTestEnv(t)
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"channels"}, 2},
		{[]string{"channels", "frob"}, 2},
		{[]string{"channels", "login"}, 2},
		{[]string{"channels", "login", "weixin", "extra"}, 2},
		{[]string{"channels", "login", "telegram"}, 1},
		{[]string{"channels", "-h"}, 0},
	} {
		if code := e.run(t, nil, tc.args...); code != tc.code {
			t.Errorf("%v exit = %d, want %d; stderr %s", tc.args, code, tc.code, e.stderr)
		}
	}
}
