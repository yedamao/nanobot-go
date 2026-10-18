//go:build unix

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The agent can replace a bootstrap file with a FIFO. Reading it must not
// block, or every later turn would hang before the run even starts.
func TestSystemPromptSkipsSpecialFiles(t *testing.T) {
	ws := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(ws, "AGENTS.md"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "SOUL.md"), []byte("be kind"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- NewContextBuilder(ws).SystemPrompt("cli") }()
	select {
	case got := <-done:
		if strings.Contains(got, "## AGENTS.md") || !strings.Contains(got, "be kind") {
			t.Errorf("prompt:\n%s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SystemPrompt blocked on a FIFO")
	}
}
