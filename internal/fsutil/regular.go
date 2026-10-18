package fsutil

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrNotRegular is returned for directories, FIFOs, devices and other
// files that are not regular.
var ErrNotRegular = errors.New("not a regular file")

// ReadRegularFile reads at most limit bytes from the regular file at path.
// It opens non-blocking and fails with ErrNotRegular for anything else, so
// a FIFO without a writer or a device cannot hang the caller.
func ReadRegularFile(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|oNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read %s: %w", path, ErrNotRegular)
	}
	return io.ReadAll(io.LimitReader(f, limit))
}
