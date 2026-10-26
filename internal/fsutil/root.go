package fsutil

import (
	"crypto/rand"
	"os"
	"path"
	"strings"
)

// WriteInRoot writes data to rel (slash-separated) inside dir through an
// os.Root, so rel cannot escape dir, via a temporary file renamed into
// place. Missing directories are created with mode 0700 and the file
// gets mode 0600.
func WriteInRoot(dir, rel string, data []byte) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(path.Dir(rel), 0o700); err != nil {
		return err
	}
	tmp := rel + ".tmp-" + strings.ToLower(rand.Text()[:8])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		root.Remove(tmp)
	}
	return err
}
