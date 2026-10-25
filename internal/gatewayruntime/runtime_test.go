package gatewayruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProcs is a process table for a Runtime that starts nothing.
type fakeProcs struct {
	mu      sync.Mutex
	alive   map[int]bool
	next    int
	spawned [][]string
	stopped []int
	// dieOnSpawn makes the next spawned process exit at once.
	dieOnSpawn bool
}

func (f *fakeProcs) set(pid int, alive bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[pid] = alive
}

func newFake(t *testing.T, workspace string) (*Runtime, *fakeProcs) {
	t.Helper()
	inst, err := Resolve(filepath.Join(t.TempDir(), "config.json"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProcs{alive: map[int]bool{100: true}, next: 5000}
	rt := New(inst)
	rt.Executable = func() (string, error) { return "/bin/nanobot", nil }
	rt.Spawn = func(argv []string, _ *os.File) (int, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.next++
		f.alive[f.next] = !f.dieOnSpawn
		f.dieOnSpawn = false
		f.spawned = append(f.spawned, argv)
		return f.next, nil
	}
	rt.Alive = func(pid int) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.alive[pid]
	}
	rt.Identity = func(pid int) string { return "id-" + strconv.Itoa(pid) }
	rt.Terminate = func(pid int, _ bool) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.alive[pid] = false
		f.stopped = append(f.stopped, pid)
		return nil
	}
	rt.Getpid = func() int { return 100 }
	rt.Sleep = func(time.Duration) {}
	rt.StartupGrace = time.Nanosecond
	return rt, f
}

func status(t *testing.T, rt *Runtime) Status {
	t.Helper()
	s, err := rt.Status()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestResolve(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	def := filepath.Join(home, ".nanobot-go", "config.json")
	inst, err := Resolve(def, "")
	if err != nil {
		t.Fatal(err)
	}
	p := inst.Paths
	if p.StatePath != filepath.Join(home, ".nanobot-go", "run", "gateway.json") ||
		p.LogPath != filepath.Join(home, ".nanobot-go", "logs", "gateway.log") ||
		p.ClientsPath != filepath.Join(home, ".nanobot-go", "run", "gateway.clients.json") {
		t.Errorf("default paths %+v", p)
	}

	other := filepath.Join(home, "other", "config.json")
	a, _ := Resolve(other, "")
	b, _ := Resolve(other, "~/ws")
	c, _ := Resolve(def, "~/ws")
	if a.Paths.StatePath != filepath.Join(home, "other", "run", "gateway."+instanceSuffix("", other)+".json") {
		t.Errorf("other config state %s", a.Paths.StatePath)
	}
	if b.Workspace != filepath.Join(home, "ws") {
		t.Errorf("workspace %q", b.Workspace)
	}
	seen := map[string]bool{p.StatePath: true}
	for _, i := range []Instance{a, b, c} {
		if seen[i.Paths.StatePath] {
			t.Errorf("state path %s collides", i.Paths.StatePath)
		}
		seen[i.Paths.StatePath] = true
		if !strings.HasSuffix(i.Paths.ClientsPath, ".clients.json") || len(filepath.Base(i.Paths.StatePath)) != len("gateway.0123456789abcdef.json") {
			t.Errorf("paths %+v", i.Paths)
		}
	}
	// nanobot's _instance_suffix: sha1("/w|/c.json")[:16].
	if got := instanceSuffix("/w", "/c.json"); got != "72246035af358ae5" {
		t.Errorf("suffix = %s", got)
	}
}

func TestStaleState(t *testing.T) {
	rt, _ := newFake(t, "")
	if err := rt.writeState(&State{PID: 4242, LaunchMode: LaunchBackground}); err != nil {
		t.Fatal(err)
	}
	s := status(t, rt)
	if s.Running || s.Reason != "stale_state" || exists(rt.Paths().StatePath) {
		t.Errorf("status %+v, state file kept %v", s, exists(rt.Paths().StatePath))
	}
	// A live PID with another identity was reused.
	rt.Alive = func(int) bool { return true }
	rt.writeState(&State{PID: 4243, Identity: "old"})
	if s := status(t, rt); s.Running {
		t.Errorf("reused pid counts as running: %+v", s)
	}
	if res, err := rt.Stop(time.Second); err != nil || res.Message != MsgNotRunning {
		t.Errorf("stop = %+v, %v", res, err)
	}
}

func TestStartBackgroundAndStop(t *testing.T) {
	rt, f := newFake(t, "/ws")
	res, err := rt.StartBackground(StartOptions{Port: 18800, Host: "127.0.0.1", Verbose: true})
	if err != nil || !res.OK || res.Message != MsgStarted {
		t.Fatalf("start = %+v, %v", res, err)
	}
	s := res.Status
	if !s.Running || s.PID != 5001 || s.Port != 18800 || s.LaunchMode != LaunchBackground || s.Lifetime != LifetimeExplicit {
		t.Errorf("status %+v", s)
	}
	want := "/bin/nanobot gateway --config " + rt.Instance.ConfigPath + " --workspace /ws --host 127.0.0.1 --port 18800 --verbose"
	if got := strings.Join(f.spawned[0], " "); got != want {
		t.Errorf("argv %q, want %q", got, want)
	}
	if !exists(rt.Paths().LogPath) {
		t.Error("no log file")
	}
	if res, _ := rt.StartBackground(StartOptions{Port: 18800}); res.OK || res.Message != MsgAlreadyRunning || res.Promoted || len(f.spawned) != 1 {
		t.Errorf("second start = %+v", res)
	}
	res, err = rt.Stop(time.Second)
	if err != nil || !res.OK || res.Status.Running || exists(rt.Paths().StatePath) {
		t.Errorf("stop = %+v, %v", res, err)
	}
	if res, _ := rt.Stop(time.Second); res.Message != MsgNotRunning {
		t.Errorf("second stop = %+v", res)
	}
}

func TestExitedDuringStartup(t *testing.T) {
	rt, f := newFake(t, "")
	f.dieOnSpawn = true
	res, err := rt.StartBackground(StartOptions{})
	if err != nil || res.OK || res.Message != MsgExitedDuringStartup || exists(rt.Paths().StatePath) {
		t.Errorf("start = %+v, %v", res, err)
	}
}

func TestRestart(t *testing.T) {
	rt, f := newFake(t, "")
	if res, _ := rt.Restart(StartOptions{}, time.Second); res.Message != MsgNotRunning {
		t.Errorf("restart idle = %+v", res)
	}
	rt.StartBackground(StartOptions{Port: 1})
	res, err := rt.Restart(StartOptions{Port: 2}, time.Second)
	if err != nil || !res.OK || res.Status.PID != 5002 || res.Status.Port != 2 || f.stopped[0] != 5001 {
		t.Errorf("restart = %+v, %v, stopped %v", res, err, f.stopped)
	}
	rt.Stop(time.Second)

	release, err := rt.Claim(StartOptions{Port: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if res, _ := rt.Restart(StartOptions{}, time.Second); res.Message != MsgForegroundRestartRequired {
		t.Errorf("restart foreground = %+v", res)
	}
}

func TestClaim(t *testing.T) {
	rt, f := newFake(t, "")
	release, err := rt.Claim(StartOptions{Port: 7})
	if err != nil {
		t.Fatal(err)
	}
	s := status(t, rt)
	if !s.Running || s.PID != 100 || s.LaunchMode != LaunchForeground || s.Port != 7 {
		t.Errorf("status %+v", s)
	}
	// Another process cannot claim it, nor start a background one.
	other := *rt
	other.Getpid = func() int { return 101 }
	f.set(101, true)
	var already *AlreadyRunningError
	if _, err := other.Claim(StartOptions{}); !errors.As(err, &already) || already.Status.PID != 100 {
		t.Errorf("second claim = %v", err)
	}
	if res, _ := other.StartBackground(StartOptions{}); res.Message != MsgAlreadyRunning || res.Status.LaunchMode != LaunchForeground {
		t.Errorf("background over foreground = %+v", res)
	}
	release()
	if exists(rt.Paths().StatePath) {
		t.Error("release kept the state")
	}

	// The background child confirms its launcher's record.
	res, _ := rt.StartBackground(StartOptions{Port: 9})
	child := *rt
	child.Getpid = func() int { return res.Status.PID }
	release, err = child.Claim(StartOptions{Port: 9})
	if err != nil {
		t.Fatal(err)
	}
	if s := status(t, rt); s.LaunchMode != LaunchBackground || s.PID != res.Status.PID {
		t.Errorf("child claim status %+v", s)
	}
	release()
	if s := status(t, rt); s.Running {
		t.Errorf("after child exit %+v", s)
	}
}

func TestLeaseOnDemand(t *testing.T) {
	rt, f := newFake(t, "")
	f.set(200, true)
	a := rt.NewClientLease("webui")
	b := rt.NewClientLease("webui")
	b.PID = 200
	if _, err := a.EnsureOnDemand(StartOptions{}); err == nil {
		t.Error("ensure before acquire succeeded")
	}
	for _, l := range []*ClientLease{a, b} {
		if err := l.Acquire(); err != nil {
			t.Fatal(err)
		}
	}
	res, err := a.EnsureOnDemand(StartOptions{Port: 1})
	if err != nil || !res.OK {
		t.Fatalf("ensure = %+v, %v", res, err)
	}
	if res, _ := b.EnsureOnDemand(StartOptions{Port: 1}); res.Message != MsgAlreadyRunning || len(f.spawned) != 1 {
		t.Errorf("second ensure = %+v", res)
	}
	s := status(t, rt)
	if s.Lifetime != LifetimeOnDemand || s.Clients != 2 {
		t.Errorf("status %+v", s)
	}
	if stopped, err := a.Release(true, time.Second); stopped || err != nil || len(f.stopped) != 0 {
		t.Errorf("first release stopped=%v err=%v", stopped, err)
	}
	if stopped, _ := a.Release(true, time.Second); stopped {
		t.Error("double release stopped")
	}
	if stopped, err := b.Release(true, time.Second); !stopped || err != nil || len(f.stopped) != 1 {
		t.Errorf("last release stopped=%v err=%v", stopped, err)
	}
	if exists(rt.Paths().ClientsPath) || status(t, rt).Running {
		t.Error("state left after the last release")
	}
}

func TestLeaseDeadClientPruned(t *testing.T) {
	rt, f := newFake(t, "")
	f.set(200, true)
	a := rt.NewClientLease("webui")
	crashed := rt.NewClientLease("webui")
	crashed.PID = 200
	a.Acquire()
	crashed.Acquire()
	a.EnsureOnDemand(StartOptions{})
	f.set(200, false) // the other client died without releasing
	if s := status(t, rt); s.Clients != 1 {
		t.Errorf("clients = %d", s.Clients)
	}
	if stopped, _ := a.Release(true, time.Second); !stopped {
		t.Error("last live client did not stop the gateway")
	}
}

func TestLeaseKeepsExplicitGateway(t *testing.T) {
	rt, f := newFake(t, "")
	l := rt.NewClientLease("webui")
	l.Acquire()
	l.EnsureOnDemand(StartOptions{})
	// `gateway --background` promotes it.
	res, err := rt.StartBackground(StartOptions{})
	if err != nil || res.Message != MsgAlreadyRunning || !res.Promoted || len(f.spawned) != 1 {
		t.Fatalf("promote = %+v, %v", res, err)
	}
	if s := status(t, rt); s.Lifetime != LifetimeExplicit {
		t.Errorf("lifetime %s", s.Lifetime)
	}
	if res, _ := rt.StartBackground(StartOptions{}); res.Promoted {
		t.Error("promoted twice")
	}
	if stopped, _ := l.Release(true, time.Second); stopped || !status(t, rt).Running {
		t.Error("release stopped an explicit gateway")
	}

	// An explicit stop forgets the leases.
	l = rt.NewClientLease("webui")
	l.Acquire()
	if res, _ := rt.Stop(time.Second); !res.OK || exists(rt.Paths().ClientsPath) {
		t.Errorf("stop = %+v, clients file kept %v", res, exists(rt.Paths().ClientsPath))
	}
}

func TestLeaseReleaseWithoutStop(t *testing.T) {
	rt, _ := newFake(t, "")
	l := rt.NewClientLease("webui")
	l.Acquire()
	l.EnsureOnDemand(StartOptions{})
	if stopped, _ := l.Release(false, time.Second); stopped || !status(t, rt).Running {
		t.Error("release without stop stopped the gateway")
	}
	if s := status(t, rt); s.Lifetime != LifetimeOnDemand || s.Clients != 0 {
		t.Errorf("status %+v", s)
	}
}

func TestLogTail(t *testing.T) {
	rt, _ := newFake(t, "")
	if lines, err := rt.LogTail(5); err != nil || lines != nil {
		t.Errorf("missing log = %v, %v", lines, err)
	}
	os.MkdirAll(rt.Paths().LogsDir, 0o700)
	os.WriteFile(rt.Paths().LogPath, []byte("a\nb\nc\n"), 0o600)
	if lines, _ := rt.LogTail(2); strings.Join(lines, ",") != "b,c" {
		t.Errorf("tail = %v", lines)
	}
	if lines, _ := rt.LogTail(10); len(lines) != 3 {
		t.Errorf("tail = %v", lines)
	}
}
