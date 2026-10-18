//go:build unix

package fsutil

import (
	"errors"
	"os"
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

func TestOpenRegularNoFollow(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	f, err := OpenRegularNoFollow(file, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("create regular file: %v", err)
	}
	f.Close()

	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenRegularNoFollow(link, os.O_RDWR|os.O_APPEND, 0o600); err == nil {
		f.Close()
		t.Error("opened a symlink")
	}

	pipe := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := OpenRegularNoFollow(pipe, os.O_RDONLY, 0)
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Errorf("FIFO: err = %v, want ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenRegularNoFollow blocked on a FIFO")
	}
}
