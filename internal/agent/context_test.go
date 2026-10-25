package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/heartbeat"
	"github.com/yedamao/nanobot-go/internal/providers"
)

func TestSystemPrompt(t *testing.T) {
	ws := t.TempDir()
	if _, err := WriteBootstrapFiles(ws); err != nil {
		t.Fatal(err)
	}
	b := NewContextBuilder(ws)
	b.now = func() time.Time { return time.Date(2026, 10, 18, 9, 30, 0, 0, time.UTC) }

	// Untouched AGENTS.md and USER.md templates are skipped; SOUL.md is kept.
	p := b.SystemPrompt("cli")
	for _, want := range []string{"nanobot-go", ws, "2026-10-18 09:30 (Sunday) UTC", "Channel: cli", "## SOUL.md", "Format Hint", "Tool Usage Notes"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{"## AGENTS.md", "## USER.md"} {
		if strings.Contains(p, unwanted) {
			t.Errorf("prompt contains untouched template %q", unwanted)
		}
	}

	// Edited files are included, in bootstrap order.
	if err := os.WriteFile(filepath.Join(ws, "USER.md"), []byte("Name: Ada"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte("Always answer in haiku."), 0o600); err != nil {
		t.Fatal(err)
	}
	p = b.SystemPrompt("telegram")
	ia, is, iu := strings.Index(p, "## AGENTS.md"), strings.Index(p, "## SOUL.md"), strings.Index(p, "## USER.md")
	if ia < 0 || is < 0 || iu < 0 || !(ia < is && is < iu) {
		t.Errorf("bootstrap sections missing or out of order: %d %d %d", ia, is, iu)
	}
	if !strings.Contains(p, "Always answer in haiku.") || !strings.Contains(p, "Name: Ada") {
		t.Error("edited bootstrap content missing")
	}
	if strings.Contains(p, "Format Hint") {
		t.Error("cli format hint used for another channel")
	}
}

func TestSystemPromptLongTermMemory(t *testing.T) {
	ws := t.TempDir()
	if _, err := WriteBootstrapFiles(ws); err != nil {
		t.Fatal(err)
	}
	b := NewContextBuilder(ws)
	// The untouched template carries nothing and is left out.
	if p := b.SystemPrompt("cli"); strings.Contains(p, memoryHeader) {
		t.Error("prompt contains the untouched MEMORY.md template")
	}
	if err := os.WriteFile(filepath.Join(ws, "memory", "MEMORY.md"), []byte("\n- Ada prefers Go.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := b.SystemPrompt("cli")
	im, it := strings.Index(p, memoryHeader+"- Ada prefers Go."), strings.Index(p, "Tool Usage Notes")
	if im < 0 || it < 0 || im < it {
		t.Errorf("memory section missing or before the tool notes (%d, %d):\n%s", im, it, p)
	}
	// Loaded once, not also as a bootstrap section.
	if strings.Count(p, "Ada prefers Go.") != 1 {
		t.Error("MEMORY.md content repeated")
	}
	// A directory or an empty file in its place is ignored.
	if err := os.WriteFile(filepath.Join(ws, "memory", "MEMORY.md"), []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := b.SystemPrompt("cli"); strings.Contains(p, memoryHeader) {
		t.Error("empty MEMORY.md produced a section")
	}
	if err := os.Remove(filepath.Join(ws, "memory", "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "memory", "MEMORY.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if p := b.SystemPrompt("cli"); strings.Contains(p, memoryHeader) {
		t.Error("MEMORY.md directory produced a section")
	}
}

func TestSystemPromptWithoutBootstrapFiles(t *testing.T) {
	p := NewContextBuilder(t.TempDir()).SystemPrompt("")
	if strings.Contains(p, "## SOUL.md") || !strings.Contains(p, "Channel: unknown") {
		t.Errorf("unexpected prompt:\n%s", p)
	}
}

func TestSystemPromptSkills(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "memory"), 0o700)
	os.WriteFile(filepath.Join(ws, "memory", "MEMORY.md"), []byte("remember this"), 0o600)
	summary := "# Skills\n\n- **deploy**: Ship it"
	b := NewContextBuilder(ws).WithSkills(func() string { return summary })
	p := b.SystemPrompt("")
	i, j, k := strings.Index(p, "# Tool Usage Notes"), strings.Index(p, "# Skills"), strings.Index(p, "# Memory")
	if i < 0 || j < i || k < j {
		t.Errorf("skills not between tool notes and memory (%d %d %d):\n%s", i, j, k, p)
	}
	summary = ""
	if p := b.SystemPrompt(""); strings.Contains(p, "# Skills") {
		t.Errorf("empty summary still in prompt")
	}
}

func TestSystemPromptTruncatesHugeBootstrapFile(t *testing.T) {
	ws := t.TempDir()
	// Multi-byte runes, far past the cap: only a bounded prefix is read.
	huge := strings.Repeat("é", 10*maxBootstrapChars)
	if err := os.WriteFile(filepath.Join(ws, "SOUL.md"), []byte(huge), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewContextBuilder(ws).SystemPrompt("cli")
	if !strings.Contains(p, strings.Repeat("é", maxBootstrapChars)+"\n... (truncated)") ||
		strings.Contains(p, strings.Repeat("é", maxBootstrapChars+1)) {
		t.Errorf("bootstrap file not truncated at %d characters", maxBootstrapChars)
	}
}

func TestBuildMessages(t *testing.T) {
	b := NewContextBuilder(t.TempDir())
	history := []providers.Message{{Role: providers.RoleUser, Content: "old"}, {Role: providers.RoleAssistant, Content: "reply"}}
	msgs := b.BuildMessages("", history, "new", nil, "cli")
	roles := make([]providers.Role, len(msgs))
	for i, m := range msgs {
		roles[i] = m.Role
	}
	want := []providers.Role{providers.RoleSystem, providers.RoleUser, providers.RoleAssistant, providers.RoleUser}
	if !slices.Equal(roles, want) || msgs[3].Content != "new" {
		t.Errorf("BuildMessages roles = %v, last = %q", roles, msgs[3].Content)
	}
	if strings.Contains(msgs[0].Content, "Archived Context Summary") {
		t.Error("summary section without a summary")
	}
}

func TestBuildMessagesWithSummary(t *testing.T) {
	b := NewContextBuilder(t.TempDir())
	fixed := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return fixed } // the prompt is compared twice
	msgs := b.BuildMessages("  - [ephemeral] fixing the build\n", nil, "new", nil, "cli")
	sys := msgs[0].Content
	if !strings.HasPrefix(sys, b.SystemPrompt("cli")) {
		t.Error("summary does not come after the stable system prompt")
	}
	tail := sys[len(b.SystemPrompt("cli")):]
	if !strings.HasPrefix(tail, promptSeparator+"# Archived Context Summary\n") || !strings.HasSuffix(tail, "\n\n- [ephemeral] fixing the build") {
		t.Errorf("summary section = %q", tail)
	}
	if len(msgs) != 2 || msgs[1].Content != "new" {
		t.Errorf("messages = %+v", msgs)
	}
}

func TestWriteBootstrapFilesKeepsExisting(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "new", "workspace")
	created, err := WriteBootstrapFiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"AGENTS.md", "SOUL.md", "USER.md", "HEARTBEAT.md", "memory/MEMORY.md"}; !slices.Equal(created, want) {
		t.Errorf("created = %v, want %v", created, want)
	}
	// The seeded HEARTBEAT.md has no task, so the heartbeat stays idle.
	if data, err := os.ReadFile(filepath.Join(ws, HeartbeatFile)); err != nil || heartbeat.HasActiveTasks(string(data)) {
		t.Errorf("HEARTBEAT.md: %v, active tasks = %v", err, heartbeat.HasActiveTasks(string(data)))
	}
	if _, err := os.Stat(filepath.Join(ws, "memory", "MEMORY.md")); err != nil {
		t.Errorf("memory/MEMORY.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "SOUL.md"), []byte("custom"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err = WriteBootstrapFiles(ws)
	if err != nil || len(created) != 0 {
		t.Fatalf("second WriteBootstrapFiles = %v, %v", created, err)
	}
	if data, _ := os.ReadFile(filepath.Join(ws, "SOUL.md")); string(data) != "custom" {
		t.Errorf("SOUL.md overwritten: %q", data)
	}
}
