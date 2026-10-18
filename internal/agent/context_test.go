package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestSystemPromptWithoutBootstrapFiles(t *testing.T) {
	p := NewContextBuilder(t.TempDir()).SystemPrompt("")
	if strings.Contains(p, "## SOUL.md") || !strings.Contains(p, "Channel: unknown") {
		t.Errorf("unexpected prompt:\n%s", p)
	}
}

func TestBuildMessages(t *testing.T) {
	b := NewContextBuilder(t.TempDir())
	history := []providers.Message{{Role: providers.RoleUser, Content: "old"}, {Role: providers.RoleAssistant, Content: "reply"}}
	msgs := b.BuildMessages(history, "new", "cli")
	roles := make([]providers.Role, len(msgs))
	for i, m := range msgs {
		roles[i] = m.Role
	}
	want := []providers.Role{providers.RoleSystem, providers.RoleUser, providers.RoleAssistant, providers.RoleUser}
	if !slices.Equal(roles, want) || msgs[3].Content != "new" {
		t.Errorf("BuildMessages roles = %v, last = %q", roles, msgs[3].Content)
	}
}

func TestWriteBootstrapFilesKeepsExisting(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "new", "workspace")
	created, err := WriteBootstrapFiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(created, BootstrapFiles()) {
		t.Errorf("created = %v", created)
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
