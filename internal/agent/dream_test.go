package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/memory"
	"github.com/yedamao/nanobot-go/internal/providers"
)

func TestDreamRunIsAnEphemeralTurn(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "SOUL.md"), []byte("Be terse."), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := memory.DreamTools(ws)
	if err != nil {
		t.Fatal(err)
	}
	p := &scripted{steps: []step{
		callTools(call("c1", "write_file", `{"path": "memory/MEMORY.md", "content": "- fact"}`)),
		reply("Updated MEMORY.md."),
	}}
	run := DreamRun(NewRunner(p), NewContextBuilder(ws), RunSpec{Model: "dream-model", MaxIterations: 5})
	out, err := run(context.Background(), "consolidate this", reg)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Completed || out.StopReason != StopCompleted || out.Reply != "Updated MEMORY.md." {
		t.Errorf("outcome = %+v", out)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "memory", "MEMORY.md")); string(data) != "- fact" {
		t.Errorf("MEMORY.md = %q", data)
	}
	req := p.reqs[0]
	if len(req.Messages) != 2 || req.Model != "dream-model" {
		t.Fatalf("request = %+v", req)
	}
	sys, user := req.Messages[0], req.Messages[1]
	if sys.Role != providers.RoleSystem || !strings.Contains(sys.Content, "Be terse.") || !strings.Contains(sys.Content, "Channel: dream") {
		t.Errorf("system prompt:\n%s", sys.Content)
	}
	if user.Role != providers.RoleUser || user.Content != "consolidate this" {
		t.Errorf("user message = %+v", user)
	}
	var names []string
	for _, d := range req.Tools {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "edit_file,list_dir,read_file,write_file" {
		t.Errorf("tools = %v", names)
	}
}

func TestDreamRunReportsIncompleteRuns(t *testing.T) {
	reg, err := memory.DreamTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The iteration budget runs out: not completed, no error.
	p := &scripted{steps: []step{callTools(call("c", "list_dir", `{}`))}}
	run := DreamRun(NewRunner(p), NewContextBuilder(t.TempDir()), RunSpec{MaxIterations: 1})
	out, err := run(context.Background(), "x", reg)
	if err != nil || out.Completed || out.StopReason != StopMaxIterations {
		t.Errorf("max iterations: %+v, %v", out, err)
	}
	// A provider failure.
	p = &scripted{steps: []step{fail(errors.New("down"))}}
	run = DreamRun(NewRunner(p), NewContextBuilder(t.TempDir()), RunSpec{})
	out, err = run(context.Background(), "x", reg)
	if err == nil || out.Completed || out.StopReason != StopError {
		t.Errorf("provider error: %+v, %v", out, err)
	}
}
