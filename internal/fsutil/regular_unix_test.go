//go:build unix

package fsutil

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadRegularFileRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadRegularFile(path, 100)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("err = %v, want ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadRegularFile blocked on a FIFO")
	}
}
