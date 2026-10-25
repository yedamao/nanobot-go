package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/cron"
	"github.com/yedamao/nanobot-go/internal/providers"
)

// cronProvider schedules a one-time job when asked to "remind me", and
// answers the job's turn when it fires.
func cronProvider(at func() time.Time) func(context.Context, providers.ChatRequest) (*providers.ChatResponse, error) {
	var mu sync.Mutex
	return func(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		last := req.Messages[len(req.Messages)-1]
		switch {
		case last.Role == providers.RoleTool:
			return &providers.ChatResponse{Content: "scheduled: " + last.Content, FinishReason: providers.FinishStop}, nil
		case last.Content == "remind me":
			args, _ := json.Marshal(map[string]any{"action": "add", "message": "stand up", "at": at().Format(time.RFC3339Nano)})
			return &providers.ChatResponse{FinishReason: providers.FinishToolCalls, ToolCalls: []providers.ToolCall{
				{ID: "c1", Name: "cron", Arguments: args},
			}}, nil
		case strings.HasSuffix(last.Content, "Cron job: stand up"):
			return &providers.ChatResponse{Content: "time to stand up!", FinishReason: providers.FinishStop}, nil
		}
		return &providers.ChatResponse{Content: "echo: " + last.Content, FinishReason: providers.FinishStop}, nil
	}
}

// A job created from a chat fires under the gateway and its reply goes
// back to that chat.
func TestGatewayRunsCronJobInOriginatingChat(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, nil)
	e.provider.chat = cronProvider(func() time.Time { return time.Now().Add(1500 * time.Millisecond) })
	fake := newFakeChat(&eventLog{})
	g := startGateway(t, e, fake)
	if out := e.stdout.String(); !strings.Contains(out, "Cron: 0 scheduled jobs") || !strings.Contains(out, "Dream: every 2h") {
		t.Errorf("banner = %q", out)
	}

	fake.receive(t, "42", "remind me")
	out := waitOutbound(t, fake)
	if out.ChatID != "42" || !strings.HasPrefix(out.Content, "scheduled: Created job 'stand up' (id: ") {
		t.Fatalf("reply = %+v", out)
	}
	if strings.Contains(out.Content, "Note:") {
		t.Errorf("gateway add carries the agent note: %q", out.Content)
	}

	fired := waitOutbound(t, fake)
	if fired.Channel != "fake" || fired.ChatID != "42" || fired.Content != "time to stand up!" {
		t.Errorf("cron reply = %+v", fired)
	}

	g.cancel()
	if code := g.wait(t); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, e.stderr)
	}
	// The one-time job is gone; Dream and the heartbeat remain as system
	// jobs.
	jobs, err := cron.NewStore(e.workspace).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != cron.DreamJobID || jobs[1].ID != cron.HeartbeatJobID || !jobs[0].System() || !jobs[1].System() {
		t.Errorf("jobs after the run = %+v", jobs)
	}
	// The cron turn is part of the chat's session.
	s, err := e.loadSession(t, "fake:42")
	if err != nil {
		t.Fatal(err)
	}
	var summary []string
	for _, m := range s.Messages {
		summary = append(summary, string(m.Role))
	}
	if got := strings.Join(summary, ","); got != "user,assistant,tool,assistant,user,assistant" {
		t.Errorf("session roles = %s", got)
	}
	if last := s.Messages[len(s.Messages)-1]; last.Content != "time to stand up!" {
		t.Errorf("last message = %+v", last)
	}
}

func TestGatewayDreamScheduleConfig(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, func(c *config.Config) { c.Agents.Defaults.Dream.Cron = "0 3 * * *" })
	g := startGateway(t, e, newFakeChat(&eventLog{}))
	g.cancel()
	g.wait(t)
	if out := e.stdout.String(); !strings.Contains(out, "Dream: cron: 0 3 * * *") || !strings.Contains(out, "Heartbeat: every 30m") {
		t.Errorf("banner = %q", out)
	}

	// Turning the schedules off removes the jobs on the next start.
	e.writeGatewayConfig(t, func(c *config.Config) {
		c.Agents.Defaults.Dream.IntervalH = 0
		c.Gateway.Heartbeat.Enabled = false
	})
	g = startGateway(t, e, newFakeChat(&eventLog{}))
	g.cancel()
	g.wait(t)
	if out := e.stdout.String(); !strings.Contains(out, "Dream: not scheduled") || !strings.Contains(out, "Heartbeat: disabled") {
		t.Errorf("banner = %q", out)
	}
	if jobs, err := cron.NewStore(e.workspace).Load(); err != nil || len(jobs) != 0 {
		t.Errorf("jobs = %+v, %v", jobs, err)
	}
}

func TestGatewayRefusesCorruptCronStore(t *testing.T) {
	e := newTestEnv(t)
	e.writeGatewayConfig(t, nil)
	path := cron.NewStore(e.workspace).Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := e.app(strings.NewReader(""))
	if code := a.run(context.Background(), []string{"gateway", "--port", "0"}); code != 1 {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(e.stderr.String(), "corrupt") {
		t.Errorf("stderr = %q", e.stderr)
	}
}

// `nanobot agent` offers the cron tool; jobs are stored for the gateway,
// with a note saying so.
func TestAgentCronToolStoresJobWithNote(t *testing.T) {
	e := newTestEnv(t)
	e.provider.chat = cronProvider(func() time.Time { return time.Now().Add(time.Hour) })
	if code := e.run(t, nil, "agent", "-m", "remind me", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if out := e.stdout.String(); !strings.Contains(out, "scheduled: Created job 'stand up'") || !strings.Contains(out, "only run while `nanobot gateway` is running") {
		t.Errorf("stdout = %q", out)
	}
	jobs, err := cron.NewStore(e.workspace).Load()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	if b := jobs[0].Binding; b == nil || b.Channel != "cli" || b.ChatID != "direct" || b.SessionKey != "cli:direct" {
		t.Errorf("binding = %+v", jobs[0].Binding)
	}
}

func addTestJob(t *testing.T, workspace string, spec cron.AddSpec) cron.Job {
	t.Helper()
	svc, err := cron.NewService(cron.ServiceConfig{Store: cron.NewStore(workspace)})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Message == "" {
		spec.Message = "stand up"
	}
	if spec.Schedule.Kind == "" {
		spec.Schedule = cron.Schedule{Kind: cron.KindEvery, Every: 90 * time.Minute}
	}
	j, err := svc.Add(spec)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestCronListAndRemove(t *testing.T) {
	e := newTestEnv(t)
	// No config file: list and remove only need the workspace (they
	// never build a provider).
	if code := e.run(t, nil, "cron", "list", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	if got := e.stdout.String(); got != "No scheduled jobs.\n" {
		t.Errorf("empty list = %q", got)
	}

	job := addTestJob(t, e.workspace, cron.AddSpec{Name: "standup",
		Binding: &cron.Binding{Channel: "fake", ChatID: "42", SessionKey: "fake:42"}})
	e.stdout = &syncBuffer{}
	if code := e.run(t, nil, "cron", "list", "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	out := e.stdout.String()
	for _, want := range []string{"ID", "SCHEDULE", job.ID, "standup", "every 90m", "fake:42"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}

	e.stdout = &syncBuffer{}
	if code := e.run(t, nil, "cron", "remove", job.ID, "--workspace", e.workspace); code != 0 {
		t.Fatalf("remove exit = %d, stderr %s", code, e.stderr)
	}
	if got := e.stdout.String(); got != fmt.Sprintf("Removed job %s.\n", job.ID) {
		t.Errorf("remove = %q", got)
	}
	if code := e.run(t, nil, "cron", "remove", "--workspace", e.workspace, job.ID); code != 1 || !strings.Contains(e.stderr.String(), "not found") {
		t.Errorf("second remove exit = %d, stderr %q", code, e.stderr)
	}
}

func TestCronUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"cron"},
		{"cron", "pause"},
		{"cron", "remove"},
		{"cron", "remove", "a", "b"},
		{"cron", "run"},
	} {
		e := newTestEnv(t)
		if code := e.run(t, nil, args...); code != 2 {
			t.Errorf("%v: exit = %d, want 2", args, code)
		}
	}
}

func TestCronRun(t *testing.T) {
	e := newTestEnv(t)
	e.provider.chat = cronProvider(time.Now)
	job := addTestJob(t, e.workspace, cron.AddSpec{Name: "standup",
		Binding: &cron.Binding{Channel: "fake", ChatID: "42", SessionKey: "fake:42"}})
	if code := e.run(t, nil, "cron", "run", job.ID, "--workspace", e.workspace); code != 0 {
		t.Fatalf("exit = %d, stderr %s", code, e.stderr)
	}
	out := e.stdout.String()
	if !strings.Contains(out, "[reply for fake:42, not delivered]\ntime to stand up!") || !strings.Contains(out, "ran (ok)") {
		t.Errorf("stdout = %q", out)
	}
	jobs, err := cron.NewStore(e.workspace).Load()
	if err != nil || len(jobs) != 1 || jobs[0].State.RunCount != 1 || jobs[0].State.LastStatus != cron.StatusOK {
		t.Errorf("jobs = %+v, %v", jobs, err)
	}

	if code := e.run(t, nil, "cron", "run", "nope", "--workspace", e.workspace); code != 1 || !strings.Contains(e.stderr.String(), "not found") {
		t.Errorf("run of a missing job: exit = %d, stderr %q", code, e.stderr)
	}

	// An unbound job is skipped, not failed.
	unbound := addTestJob(t, e.workspace, cron.AddSpec{Name: "orphan"})
	e.stdout = &syncBuffer{}
	if code := e.run(t, nil, "cron", "run", unbound.ID, "--workspace", e.workspace); code != 0 || !strings.Contains(e.stdout.String(), "skipped") {
		t.Errorf("unbound run: exit = %d, stdout %q, stderr %q", code, e.stdout, e.stderr)
	}
}
