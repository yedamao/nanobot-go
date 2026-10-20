package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
)

const fakeBotToken = "123456:fake-bot-token-SECRET"

// fakeBotAPI serves getMe, getUpdates (from a queue of batches, long
// polling otherwise) and records sendMessage texts.
type fakeBotAPI struct {
	srv     *httptest.Server
	updates chan string // JSON arrays of updates
	sent    chan map[string]any
}

func newFakeBotAPI(t *testing.T) *fakeBotAPI {
	t.Helper()
	f := &fakeBotAPI{updates: make(chan string, 8), sent: make(chan map[string]any, 8)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ok := strings.CutPrefix(r.URL.Path, "/bot"+fakeBotToken+"/")
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		result := "true"
		switch method {
		case "getMe":
			result = `{"id":999,"is_bot":true,"first_name":"Nano","username":"nano_bot"}`
		case "getUpdates":
			select {
			case result = <-f.updates:
			case <-r.Context().Done():
				return
			}
		case "sendMessage":
			f.sent <- body
			result = `{"message_id":1}`
		}
		fmt.Fprintf(w, `{"ok":true,"result":%s}`, result)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// dm queues a private message from user id.
func (f *fakeBotAPI) dm(updateID, userID int, text string) {
	f.updates <- fmt.Sprintf(`[{"update_id":%d,"message":{"message_id":%d,"from":{"id":%d,"is_bot":false,"first_name":"U"},"chat":{"id":%d,"type":"private"},"text":%q}}]`,
		updateID, updateID, userID, userID, text)
}

func (f *fakeBotAPI) waitSent(t *testing.T) (chatID float64, text string) {
	t.Helper()
	select {
	case body := <-f.sent:
		chatID, _ = body["chat_id"].(float64)
		text, _ = body["text"].(string)
		return chatID, text
	case <-time.After(10 * time.Second):
		t.Fatal("nothing sent to Telegram")
		return 0, ""
	}
}

func TestGatewayRunsTelegramWithPairing(t *testing.T) {
	e := newTestEnv(t)
	api := newFakeBotAPI(t)
	e.writeGatewayConfig(t, func(c *config.Config) {
		delete(c.Channels, "fake")
		c.Channels["telegram"] = json.RawMessage(`{"enabled": true, "allowFrom": ["42"], "pollTimeout": 1, "apiBase": "` + api.srv.URL + `"}`)
	})
	e.env["NANOBOT_GO_TELEGRAM_TOKEN"] = fakeBotToken
	g := startGateway(t, e, nil)
	if !strings.Contains(e.stdout.String(), "Channels: telegram") {
		t.Errorf("banner = %q", e.stdout)
	}

	// An allowed sender gets the agent's reply.
	api.dm(1, 42, "hello")
	if chat, text := api.waitSent(t); chat != 42 || text != "echo: hello" {
		t.Errorf("reply to %v: %q", chat, text)
	}

	// An unknown sender gets a pairing code; approving it lets them in.
	api.dm(2, 7, "let me in")
	chat, text := api.waitSent(t)
	code := regexp.MustCompile(`[A-Z0-9]{4}-[A-Z0-9]{4}`).FindString(text)
	if chat != 7 || code == "" {
		t.Fatalf("pairing reply to %v: %q", chat, text)
	}
	if code := e.run(t, nil, "pairing", "list", "--config", e.configPath); code != 0 {
		t.Fatalf("pairing list exit = %d: %s", code, e.stderr)
	}
	if out := e.stdout.String(); !strings.Contains(out, code) || !strings.Contains(out, "telegram") {
		t.Errorf("pairing list does not show %s:\n%s", code, out)
	}
	if code := e.run(t, nil, "pairing", "approve", strings.ToLower(code), "--config", e.configPath); code != 0 {
		t.Fatalf("pairing approve exit = %d: %s", code, e.stderr)
	}
	api.dm(3, 7, "thanks")
	if chat, text := api.waitSent(t); chat != 7 || text != "echo: thanks" {
		t.Errorf("reply after approval to %v: %q", chat, text)
	}

	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	if s, err := e.loadSession(t, "telegram:42"); err != nil || len(s.Messages) != 2 {
		t.Errorf("session telegram:42 = %+v, %v", s, err)
	}
	for name, out := range map[string]string{"stdout": e.stdout.String(), "stderr": e.stderr.String()} {
		if strings.Contains(out, "SECRET") {
			t.Errorf("%s leaks the bot token:\n%s", name, out)
		}
	}
}

func TestGatewayTelegramBadToken(t *testing.T) {
	e := newTestEnv(t)
	api := newFakeBotAPI(t)
	e.writeGatewayConfig(t, func(c *config.Config) {
		delete(c.Channels, "fake")
		c.Channels["telegram"] = json.RawMessage(`{"enabled": true, "token": "1:WRONG-SECRET", "apiBase": "` + api.srv.URL + `"}`)
	})
	g := startGateway(t, e, nil)
	// A channel that fails to start is logged and left out.
	if !strings.Contains(e.stdout.String(), "Channels: none running") || !strings.Contains(e.stderr.String(), "rejected") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", e.stdout, e.stderr)
	}
	g.cancel()
	g.wait(t)
	if strings.Contains(e.stderr.String(), "WRONG-SECRET") {
		t.Errorf("stderr leaks the token:\n%s", e.stderr)
	}
}

func TestPairingCommand(t *testing.T) {
	e := newTestEnv(t)
	cfgFlag := []string{"--config", e.configPath}
	run := func(args ...string) int { return e.run(t, nil, append(args, cfgFlag...)...) }

	if code := run("pairing", "list"); code != 0 || !strings.Contains(e.stdout.String(), "No pending pairing requests.") {
		t.Errorf("empty list: exit %d, stdout %q, stderr %q", code, e.stdout, e.stderr)
	}
	for _, args := range [][]string{
		{"pairing", "approve", "ZZZZ-ZZZZ"},
		{"pairing", "deny", "ZZZZ-ZZZZ"},
		{"pairing", "revoke", "telegram", "42"},
	} {
		if code := run(args...); code != 1 {
			t.Errorf("%v exit = %d, want 1", args, code)
		}
	}
	for _, args := range [][]string{
		{"pairing"},
		{"pairing", "frob"},
		{"pairing", "approve"},
		{"pairing", "revoke", "telegram"},
	} {
		if code := e.run(t, nil, args...); code != 2 {
			t.Errorf("%v exit = %d, want 2", args, code)
		}
	}
	if !strings.Contains(e.stderr.String(), "invalid or expired pairing code ZZZZ-ZZZZ") {
		t.Errorf("stderr = %s", e.stderr)
	}
}
