//go:build unix

package memory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/fsutil"
)

// The history lock is held across processes: another holder (simulated
// with a separate open file description) blocks appends until released.
func TestHistoryLockBlocksOtherHolders(t *testing.T) {
	s := newTestStore(t)
	appendN(t, s, 1)
	other, err := os.OpenFile(filepath.Join(filepath.Dir(s.HistoryPath()), lockFile), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := fsutil.LockExclusive(other, false); err != nil {
		t.Fatal(err)
	}
	// While held, a non-waiting attempt fails with errLocked.
	probe, err := os.OpenFile(other.Name(), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.LockExclusive(probe, false); !errors.Is(err, errLocked) {
		t.Errorf("second lock = %v, want errLocked", err)
	}
	probe.Close()

	done := make(chan error, 1)
	go func() {
		_, err := s.Append("waits", "")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Append did not wait for the lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	other.Close() // release
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Append still blocked after the lock was released")
	}
	if got := len(cursors(t, s)); got != 2 {
		t.Errorf("entries = %d", got)
	}
}
