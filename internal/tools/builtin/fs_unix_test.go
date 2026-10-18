//go:build unix

package builtin

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/tools"
)

// A FIFO in the workspace has no writer, so a blocking open would hang the
// turn forever. The tools must refuse non-regular files instead.
func TestFileToolsRejectFIFO(t *testing.T) {
	ws := newWS(t)
	if err := syscall.Mkfifo(filepath.Join(ws.Root, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	cases := []struct {
		tool tools.Tool
		args map[string]any
	}{
		{NewReadFile(ws), map[string]any{"path": "pipe"}},
		{NewEditFile(ws), map[string]any{"path": "pipe", "old_string": "a", "new_string": "b"}},
	}
	for _, c := range cases {
		done := make(chan tools.Result, 1)
		go func() { done <- run(t, c.tool, c.args) }()
		select {
		case res := <-done:
			if !res.IsError || !strings.Contains(res.Content, "not a regular file") {
				t.Errorf("%s: got %+v", c.tool.Name(), res)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s blocked on a FIFO", c.tool.Name())
		}
	}
}
