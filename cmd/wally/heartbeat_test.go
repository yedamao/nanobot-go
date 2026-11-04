package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/heartbeat"
	"github.com/yedamao/nanobot-go/internal/providers"
)

// heartbeatProvider answers heartbeat turns with reply, the notification
// check with notify, and echoes everything else. evaluated is signaled on
// every check.
func heartbeatProvider(reply string, notify bool, evaluated chan<- struct{}) func(context.Context, providers.ChatRequest) (*providers.ChatResponse, error) {
	return func(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
		last := req.Messages[len(req.Messages)-1]
		switch {
		case len(req.Tools) == 1 && req.Tools[0].Name == "evaluate_notification":
			select {
			case evaluated <- struct{}{}:
			default:
			}
			args, _ := json.Marshal(map[string]any{"should_notify": notify})
			return &providers.ChatResponse{FinishReason: providers.FinishToolCalls, ToolCalls: []providers.ToolCall{
				{ID: "e1", Name: "evaluate_notification", Arguments: args},
			}}, nil
		case strings.Contains(last.Content, "You are executing periodic heartbeat tasks."):
			return &providers.ChatResponse{Content: reply, FinishReason: providers.FinishStop}, nil
		}
		return &providers.ChatResponse{Content: "echo: " + last.Content, FinishReason: providers.FinishStop}, nil
	}
}

func startHeartbeatGateway(t *testing.T, reply string, notify bool) (*testEnv, *fakeChat, *gatewayRun, chan struct{}) {
	t.Helper()
	e := newTestEnv(t)
	e.writeGatewayConfig(t, func(c *config.Config) { c.Gateway.Heartbeat.IntervalS = 1 })
	if err := os.MkdirAll(e.workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.workspace, heartbeat.File), []byte("## Active Tasks\n- check the build\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	evaluated := make(chan struct{}, 1)
	e.provider.chat = heartbeatProvider(reply, notify, evaluated)
	fake := newFakeChat(&eventLog{})
	g := startGateway(t, e, fake)
	if out := e.stdout.String(); !strings.Contains(out, "Heartbeat: every 1s") {
		t.Errorf("banner = %q", out)
	}
	// A chat to report to.
	fake.receive(t, "42", "hi")
	if out := waitOutbound(t, fake); out.Content != "echo: hi" {
		t.Fatalf("reply = %+v", out)
	}
	return e, fake, g, evaluated
}

// A result the check deems worth telling goes to the most recent chat and
// joins its history; the turn itself runs in the heartbeat session.
func TestGatewayHeartbeatNotifies(t *testing.T) {
	e, fake, g, _ := startHeartbeatGateway(t, "The build is red: 3 tests fail.", true)
	out := waitOutbound(t, fake)
	if out.Channel != "fake" || out.ChatID != "42" || out.Content != "The build is red: 3 tests fail." {
		t.Errorf("heartbeat message = %+v", out)
	}
	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	s, err := e.loadSession(t, "fake:42")
	if err != nil {
		t.Fatal(err)
	}
	if last := s.Messages[len(s.Messages)-1]; last.Role != providers.RoleAssistant || last.Content != "The build is red: 3 tests fail." {
		t.Errorf("chat's last message = %+v", last)
	}
	hb, err := e.loadSession(t, heartbeat.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(hb.Messages) < 2 || !strings.Contains(hb.Messages[0].Content, "- check the build") {
		t.Errorf("heartbeat session = %+v", hb.Messages)
	}
}

// A routine result stays silent.
func TestGatewayHeartbeatSilenced(t *testing.T) {
	e, fake, g, evaluated := startHeartbeatGateway(t, "All clear.", false)
	deadline := time.After(10 * time.Second)
	for checks := 0; checks < 2; { // two runs, so the first one's delivery would have shown
		select {
		case <-evaluated:
			checks++
		case out := <-fake.sent:
			t.Fatalf("heartbeat delivered %+v", out)
		case <-deadline:
			t.Fatal("heartbeat did not run")
		}
	}
	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	select {
	case out := <-fake.sent:
		t.Errorf("heartbeat delivered %+v", out)
	default:
	}
	s, err := e.loadSession(t, "fake:42")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 {
		t.Errorf("chat history = %+v", s.Messages)
	}
}
