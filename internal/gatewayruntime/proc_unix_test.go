//go:build unix

package gatewayruntime

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestRealProcess detaches a real child (sleep), checks its session,
// and stops it.
func TestRealProcess(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	inst, err := Resolve(filepath.Join(t.TempDir(), "config.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	rt := New(inst)
	rt.Executable = func() (string, error) { return "unused", nil }
	rt.Command = func(string, ...string) *exec.Cmd { return exec.Command(sleep, "30") }
	res, err := rt.StartBackground(StartOptions{})
	if err != nil || !res.OK {
		t.Fatalf("start = %+v, %v", res, err)
	}
	pid := res.Status.PID
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	if sid, err := syscall.Getsid(pid); err != nil || sid != pid {
		t.Errorf("child session = %d, %v; want its own (%d)", sid, err, pid)
	}
	if id := processIdentity(pid); id == "" {
		t.Error("no identity")
	}
	res, err = rt.Stop(5 * time.Second)
	if err != nil || !res.OK || processAlive(pid) {
		t.Fatalf("stop = %+v, %v, alive %v", res, err, processAlive(pid))
	}
}
