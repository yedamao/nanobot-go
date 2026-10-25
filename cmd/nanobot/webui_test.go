package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	wschan "github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
)

// enableWebUI replaces the fake channel with the websocket channel.
func enableWebUI(c *config.Config) {
	delete(c.Channels, "fake")
	c.Channels[wschan.Name] = json.RawMessage(`{"enabled": true}`)
}

func readWSFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) wschan.Frame {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var f wschan.Frame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("frame %q: %v", data, err)
	}
	return f
}

// TestGatewayWebUIRoundTrip runs one browser chat turn through the real
// agent loop (with the echoing fake provider) on the gateway's address.
func TestGatewayWebUIRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, enableWebUI)
	g := startGateway(t, e, nil)

	if !strings.Contains(e.stdout.String(), "WebUI: http://"+g.addr+"/") ||
		!strings.Contains(e.stdout.String(), "Channels: websocket") {
		t.Errorf("banner = %q", e.stdout)
	}
	// Health and the page share the address.
	if code, body := get(t, "http://"+g.addr+"/healthz"); code != http.StatusOK || body != healthBody {
		t.Errorf("GET /healthz = %d %q", code, body)
	}
	if code, body := get(t, "http://"+g.addr+"/"); code != http.StatusOK || !strings.Contains(body, "app.js") {
		t.Errorf("GET / = %d %q", code, body)
	}
	code, body := get(t, "http://"+g.addr+wschan.StatusPath)
	var st wschan.Status
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil {
		t.Fatalf("GET %s = %d %q: %v", wschan.StatusPath, code, body, err)
	}
	if st.Version != version || st.Model == "" ||
		!slices.Contains(st.Channels, wschan.ChannelStatus{Name: wschan.Name, Enabled: true, Running: true}) {
		t.Errorf("status = %+v", st)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+g.addr+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	hello := readWSFrame(t, ctx, conn)
	if hello.Type != wschan.TypeHello || hello.ChatID == "" {
		t.Fatalf("hello = %+v", hello)
	}
	if h := readWSFrame(t, ctx, conn); h.Type != wschan.TypeHistory || len(h.Messages) != 0 {
		t.Fatalf("history of a new chat = %+v", h)
	}
	// The socket outlives the health endpoint's 2-second timeouts.
	time.Sleep(healthReadTimeout + 500*time.Millisecond)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"chat","content":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	reply := readWSFrame(t, ctx, conn)
	if reply.Type != wschan.TypeMessage || reply.Role != "assistant" || reply.Content != "echo: hello" {
		t.Errorf("reply = %+v", reply)
	}

	// A reload resumes the chat and gets the turn back from the store.
	again, _, err := websocket.Dial(ctx, "ws://"+g.addr+"/ws?chat_id="+hello.ChatID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer again.CloseNow()
	if h := readWSFrame(t, ctx, again); h.Type != wschan.TypeHello || h.ChatID != hello.ChatID {
		t.Fatalf("resumed hello = %+v", h)
	}
	want := []wschan.HistoryMessage{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "echo: hello"}}
	if h := readWSFrame(t, ctx, again); h.Type != wschan.TypeHistory || !slices.Equal(h.Messages, want) {
		t.Errorf("resumed history = %+v, want %+v", h, want)
	}
	again.Close(websocket.StatusNormalClosure, "")

	// Shutdown closes the socket with "going away".
	readErr := make(chan error, 1)
	go func() {
		_, _, err := conn.Read(context.Background())
		readErr <- err
	}()
	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	if err := <-readErr; websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Errorf("socket after shutdown: %v", err)
	}
	if s, err := e.loadSession(t, "websocket:"+hello.ChatID); err != nil || len(s.Messages) != 2 {
		t.Errorf("session websocket:%s = %+v, %v", hello.ChatID, s, err)
	}
}

// TestGatewayWebUISettings saves a setting through the running gateway's
// /ui/settings: the file changes, the running gateway does not.
func TestGatewayWebUISettings(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, func(c *config.Config) {
		enableWebUI(c)
		c.Providers.Anthropic.APIKey = "sk-settings-test-key"
	})
	g := startGateway(t, e, nil)
	url := "http://" + g.addr + wschan.SettingsPath

	code, body := get(t, url)
	var v wschan.SettingsView
	if err := json.Unmarshal([]byte(body), &v); code != http.StatusOK || err != nil || !v.Settings.Tools.Exec {
		t.Fatalf("GET %s = %d %q: %v", wschan.SettingsPath, code, body, err)
	}
	if strings.Contains(body, "sk-settings-test-key") {
		t.Errorf("settings expose the key: %s", body)
	}
	patch := func(body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	if code, body := patch(`{"providers":{"anthropic":{"apiKey":"x"}}}`); code != http.StatusBadRequest {
		t.Errorf("secret PATCH = %d %q", code, body)
	}
	code, body = patch(`{"tools":{"exec":false}}`)
	if err := json.Unmarshal([]byte(body), &v); code != http.StatusOK || err != nil ||
		v.Settings.Tools.Exec || !slices.Equal(v.RestartRequired, []string{"tools.exec"}) {
		t.Fatalf("PATCH = %d %q: %v", code, body, err)
	}
	saved, err := config.Load(e.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Tools.Exec.Enable || saved.Providers.Anthropic.APIKey != "sk-settings-test-key" {
		t.Errorf("saved config: exec %v, key kept %v", saved.Tools.Exec.Enable, saved.Providers.Anthropic.APIKey != "")
	}
	// The status still reports what the gateway runs with.
	_, body = get(t, "http://"+g.addr+wschan.StatusPath)
	var st wschan.Status
	if err := json.Unmarshal([]byte(body), &st); err != nil || !st.Tools.Exec {
		t.Errorf("status after save = %+v, %v", st, err)
	}
	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
}

func TestGatewayWebUIDisabled(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, func(c *config.Config) {
		c.Channels[wschan.Name] = json.RawMessage(`{"enabled": false}`)
	})
	g := startGateway(t, e, newFakeChat(&eventLog{}))
	for _, path := range []string{"/", "/ui/", "/ws", wschan.StatusPath} {
		if code, _ := get(t, "http://"+g.addr+path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
	if strings.Contains(e.stdout.String(), "WebUI:") {
		t.Errorf("banner mentions the WebUI: %q", e.stdout)
	}
	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
}
