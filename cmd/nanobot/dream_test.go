package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yedamao/nanobot-go/internal/memory"
	"github.com/yedamao/nanobot-go/internal/providers"
)

func appendHistory(t *testing.T, workspace string, contents ...string) {
	t.Helper()
	store := memory.NewStore(workspace)
	for _, c := range contents {
		if _, err := store.Append(c, "cli:direct"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDreamCommand(t *testing.T) {
	e := newTestEnv(t)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if err := os.MkdirAll(e.workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.workspace, "AGENTS.md"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	appendHistory(t, e.workspace, "- [permanent] The user's name is Ada.", "- [durable] Ada deploys with make ship.")

	var mu sync.Mutex
	var reqs []providers.ChatRequest
	e.provider.chat = func(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		reqs = append(reqs, req)
		if len(reqs) == 1 {
			return &providers.ChatResponse{FinishReason: providers.FinishToolCalls, ToolCalls: []providers.ToolCall{
				{ID: "c1", Name: "write_file", Arguments: json.RawMessage(`{"path":"USER.md","content":"Name: Ada"}`)},
				{ID: "c2", Name: "write_file", Arguments: json.RawMessage(`{"path":"AGENTS.md","content":"pwned"}`)},
				{ID: "c3", Name: "exec", Arguments: json.RawMessage(`{"command":"echo hi"}`)},
			}}, nil
		}
		return &providers.ChatResponse{Content: "Saved the user's name to USER.md.", FinishReason: providers.FinishStop}, nil
	}

	if code := e.run(t, nil, "dream", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	out := e.stdout.String()
	for _, want := range []string{"processed 2 of 2 new history entries (cursor 2)", "Saved the user's name to USER.md."} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(e.workspace, "USER.md")); string(data) != "Name: Ada" {
		t.Errorf("USER.md = %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(e.workspace, "AGENTS.md")); string(data) != "keep" {
		t.Errorf("AGENTS.md = %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(e.workspace, "memory", ".dream_cursor")); strings.TrimSpace(string(data)) != "2" {
		t.Errorf(".dream_cursor = %q", data)
	}

	// The model saw the history and only the restricted tools; the
	// forbidden calls came back as errors.
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if user := reqs[0].Messages[1].Content; !strings.Contains(user, "## Conversation History") || !strings.Contains(user, "make ship") {
		t.Errorf("dream prompt:\n%s", user)
	}
	var names []string
	for _, d := range reqs[0].Tools {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "edit_file,list_dir,read_file,write_file" {
		t.Errorf("tools = %v", names)
	}
	results := reqs[1].Messages[len(reqs[1].Messages)-3:]
	if results[0].IsError || !results[1].IsError || !results[2].IsError || !strings.Contains(results[2].Content, "not found") {
		t.Errorf("tool results = %+v", results)
	}
	// Nothing was saved as a session.
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.configPath), "sessions")); !os.IsNotExist(err) {
		t.Errorf("sessions dir created: %v", err)
	}

	if _, err := exec.LookPath("git"); err == nil {
		if !strings.Contains(out, "Committed memory changes: ") {
			t.Errorf("no commit reported:\n%s", out)
		}
		log, err := exec.Command("git", "-C", e.workspace, "log", "--format=%s", "--name-only").CombinedOutput()
		if err != nil || !strings.HasPrefix(string(log), "dream: ") || !strings.Contains(string(log), "USER.md") {
			t.Errorf("git log = %s, %v", log, err)
		}
	}

	// A second run has nothing to do and makes no request.
	e.stdout = &syncBuffer{}
	if code := e.run(t, nil, "dream", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if out := e.stdout.String(); !strings.Contains(out, "nothing to process (cursor 2)") || len(reqs) != 2 {
		t.Errorf("second run: %q, %d requests", out, len(reqs))
	}
}

func TestDreamCommandFailureKeepsCursor(t *testing.T) {
	e := newTestEnv(t)
	if err := os.MkdirAll(e.workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	appendHistory(t, e.workspace, "- fact")
	e.provider.chat = func(context.Context, providers.ChatRequest) (*providers.ChatResponse, error) {
		return nil, &providers.APIError{Provider: "fake", Kind: providers.KindAuth, Message: "bad key"}
	}
	if code := e.run(t, nil, "dream", "--workspace", e.workspace); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(e.stderr.String(), "bad key") || !strings.Contains(e.stdout.String(), "did not complete") {
		t.Errorf("stdout %q, stderr %q", e.stdout, e.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.workspace, "memory", ".dream_cursor")); !os.IsNotExist(err) {
		t.Errorf("cursor written after a failed run: %v", err)
	}
}

func TestDreamCommandEmptyWorkspace(t *testing.T) {
	e := newTestEnv(t)
	e.provider.chat = func(context.Context, providers.ChatRequest) (*providers.ChatResponse, error) {
		t.Error("provider called with no history")
		return nil, nil
	}
	if code := e.run(t, nil, "dream", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if out := e.stdout.String(); !strings.Contains(out, "nothing to process (cursor 0)") {
		t.Errorf("stdout = %q", out)
	}
	if code := e.run(t, nil, "dream", "extra"); code != 2 {
		t.Errorf("extra argument exit = %d, want 2", code)
	}
}
