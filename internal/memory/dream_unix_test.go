//go:build unix

package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/fsutil"
)

// fakeGit writes a shell script standing in for git.
func fakeGit(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestDreamGitFailureIsReportedAfterCursorMoves(t *testing.T) {
	d, agent := newTestDream(t, DreamConfig{Git: fakeGit(t, "echo broken >&2\nexit 3"), MaxHistoryEntries: 1, BatchEntries: 5})
	appendN(t, d.cfg.Store, 3)
	agent.edit = writeMemory("- fact")
	res, err := d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("err = %v", err)
	}
	// The run itself succeeded: the cursor moves and history is trimmed.
	if res.Cursor != 3 || res.Dropped != 2 {
		t.Errorf("result = %+v", res)
	}
}

func TestDreamGitTimeout(t *testing.T) {
	d, _ := newTestDream(t, DreamConfig{Git: fakeGit(t, "exec sleep 10"), GitTimeout: 100 * time.Millisecond})
	appendN(t, d.cfg.Store, 1)
	start := time.Now()
	res, err := d.Run(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("git timeout not enforced: %v", time.Since(start))
	}
	if res.Cursor != 1 {
		t.Errorf("cursor = %d", res.Cursor)
	}
}

func TestDreamRefusesConcurrentRun(t *testing.T) {
	d, agent := newTestDream(t, DreamConfig{})
	appendN(t, d.cfg.Store, 1)
	// Another process holds the Dream lock.
	other, err := os.OpenFile(filepath.Join(filepath.Dir(d.cfg.Store.HistoryPath()), dreamLockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.LockExclusive(other, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Errorf("err = %v", err)
	}
	if len(agent.prompts) != 0 {
		t.Error("ran while another Dream held the lock")
	}
	other.Close()
	if res, err := d.Run(context.Background()); err != nil || res.Cursor != 1 {
		t.Errorf("Run after release = %+v, %v", res, err)
	}
}
