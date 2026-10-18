package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestReadRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRegularFile(path, 100); err != nil || string(got) != "hello world" {
		t.Errorf("ReadRegularFile = %q, %v", got, err)
	}
	if got, err := ReadRegularFile(path, 5); err != nil || string(got) != "hello" {
		t.Errorf("limited ReadRegularFile = %q, %v", got, err)
	}
	if _, err := ReadRegularFile(dir, 100); !errors.Is(err, ErrNotRegular) {
		t.Errorf("directory: err = %v, want ErrNotRegular", err)
	}
	if _, err := ReadRegularFile(filepath.Join(dir, "missing"), 100); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: err = %v, want ErrNotExist", err)
	}
}
