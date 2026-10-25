package gatewayruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/fsutil"
)

// Launch modes recorded in the state file.
const (
	LaunchForeground = "foreground" // attached to the terminal that ran `nanobot gateway`
	LaunchBackground = "background" // detached by this package
	LaunchUnknown    = "unknown"
)

// Lifetimes: an on-demand gateway stops when its last client lease is
// released; an explicit one runs until `nanobot gateway stop`.
const (
	LifetimeOnDemand = "on_demand"
	LifetimeExplicit = "explicit"
)

// Result messages, as in nanobot.
const (
	MsgStarted                   = "gateway_started_background"
	MsgAlreadyRunning            = "gateway_already_running"
	MsgNotRunning                = "gateway_not_running"
	MsgStopped                   = "gateway_stopped"
	MsgStateStale                = "gateway_state_stale"
	MsgExitedDuringStartup       = "gateway_exited_during_startup"
	MsgStopTimeout               = "gateway_stop_timeout"
	MsgForegroundRestartRequired = "gateway_foreground_restart_required"
)

const (
	defaultStartupGrace = 200 * time.Millisecond
	// killWait bounds waiting for the exit after SIGKILL.
	killWait = 2 * time.Second
	// logTailBytes bounds how much of the log LogTail reads.
	logTailBytes = 256 << 10
)

// StartOptions are the `nanobot gateway` flags of a started gateway. The
// config file and workspace come from the Instance.
type StartOptions struct {
	Host    string // --host, when set
	Port    int    // --port, when positive
	Verbose bool
}

// State is the state file: the gateway process this instance runs.
type State struct {
	PID        int      `json:"pid"`
	StartedAt  string   `json:"started_at"`
	Platform   string   `json:"platform,omitempty"`
	Host       string   `json:"host,omitempty"`
	Port       int      `json:"port,omitempty"`
	ConfigPath string   `json:"config_path,omitempty"`
	Workspace  string   `json:"workspace,omitempty"`
	Command    []string `json:"command,omitempty"`
	LogPath    string   `json:"log_path,omitempty"`
	LaunchMode string   `json:"launch_mode,omitempty"`
	// Identity tells a live PID apart from a reused one: the process
	// group and, where available, the start time.
	Identity string `json:"identity,omitempty"`
}

// Status is a snapshot of the instance.
type Status struct {
	Running    bool
	PID        int
	Host       string
	Port       int
	StartedAt  string
	Command    []string
	LaunchMode string
	Lifetime   string
	Clients    int
	// Reason is running, not_started, stale_state, stopped and so on.
	Reason    string
	StatePath string
	LogPath   string
}

// Result is the outcome of a lifecycle operation. Message is one of the
// Msg constants; OK is false for MsgAlreadyRunning and MsgNotRunning too.
type Result struct {
	OK      bool
	Message string
	Status  Status
	// Promoted reports that StartBackground turned an on-demand gateway
	// into an explicit one.
	Promoted bool
}

// AlreadyRunningError is Claim's error when another live process holds
// the instance.
type AlreadyRunningError struct{ Status Status }

func (e *AlreadyRunningError) Error() string {
	s := e.Status
	msg := fmt.Sprintf("a nanobot gateway for this config is already running (pid %d, %s", s.PID, s.LaunchMode)
	if s.Port > 0 {
		msg += fmt.Sprintf(", port %d", s.Port)
	}
	return msg + ")"
}

// Runtime manages one instance's gateway process. The function fields
// replace process operations in tests; nil uses the real ones.
type Runtime struct {
	Instance Instance

	// Executable is the nanobot binary the background gateway runs.
	Executable func() (string, error)
	// Command builds the child (before it is detached and its output
	// redirected); Spawn replaces starting it altogether.
	Command func(name string, args ...string) *exec.Cmd
	Spawn   func(argv []string, log *os.File) (pid int, err error)
	// Alive probes a PID, Identity describes it (empty when unknown) and
	// Terminate signals it (SIGTERM, or SIGKILL with force), its whole
	// process group when it leads one.
	Alive     func(pid int) bool
	Identity  func(pid int) string
	Terminate func(pid int, force bool) error
	Getpid    func() int
	Sleep     func(time.Duration)
	Now       func() time.Time
	// StartupGrace is how long a started gateway must stay alive to count
	// as started (default 200ms).
	StartupGrace time.Duration
}

// New returns the Runtime of inst.
func New(inst Instance) *Runtime { return &Runtime{Instance: inst} }

// Paths returns the instance's files.
func (r *Runtime) Paths() Paths { return r.Instance.Paths }

func (r *Runtime) alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if r.Alive != nil {
		return r.Alive(pid)
	}
	return processAlive(pid)
}

func (r *Runtime) identity(pid int) string {
	if r.Identity != nil {
		return r.Identity(pid)
	}
	return processIdentity(pid)
}

// sameProcess reports whether pid is still the process recorded as
// identity. Unknown identities match: a live PID is then trusted.
func (r *Runtime) sameProcess(identity string, pid int) bool {
	if identity == "" {
		return true
	}
	cur := r.identity(pid)
	return cur == "" || cur == identity
}

func (r *Runtime) terminate(pid int, force bool) error {
	if r.Terminate != nil {
		return r.Terminate(pid, force)
	}
	return terminateProcess(pid, force)
}

func (r *Runtime) getpid() int {
	if r.Getpid != nil {
		return r.Getpid()
	}
	return os.Getpid()
}

func (r *Runtime) sleep(d time.Duration) {
	if r.Sleep != nil {
		r.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (r *Runtime) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// lock takes an exclusive flock on path, creating it.
func lock(path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := fsutil.LockExclusive(f, true); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { f.Close() }, nil
}

// locked runs fn holding the given locks, in order.
func locked[T any](paths []string, fn func() (T, error)) (T, error) {
	for i := range paths {
		unlock, err := lock(paths[i])
		if err != nil {
			var zero T
			return zero, err
		}
		defer unlock()
	}
	return fn()
}

func (r *Runtime) readState() *State {
	data, err := os.ReadFile(r.Paths().StatePath)
	if err != nil {
		return nil
	}
	var s State
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	return &s
}

func (r *Runtime) writeState(s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(r.Paths().StatePath, append(data, '\n'), 0o600)
}

func (r *Runtime) clearState() {
	os.Remove(r.Paths().StatePath)
}

// Status reports whether the instance's gateway runs, clearing a state
// file whose process is gone.
func (r *Runtime) Status() (Status, error) {
	return locked([]string{r.Paths().stateLock()}, func() (Status, error) {
		return r.statusLocked(""), nil
	})
}

// Settled is Status once a lifecycle operation in progress (a restart
// run by another process, say) has finished.
func (r *Runtime) Settled() (Status, error) {
	p := r.Paths()
	return locked([]string{p.transitionLock(), p.stateLock()}, func() (Status, error) {
		return r.statusLocked(""), nil
	})
}

// statusLocked is Status with the state lock held. reason, when set,
// replaces the not-running reasons.
func (r *Runtime) statusLocked(reason string) Status {
	p := r.Paths()
	s := Status{StatePath: p.StatePath, LogPath: p.LogPath, Reason: "not_started", LaunchMode: LaunchUnknown, Lifetime: LifetimeExplicit}
	c := r.liveClients()
	s.Clients = len(c.Clients)
	if c.AutoStop {
		s.Lifetime = LifetimeOnDemand
	}
	st := r.readState()
	switch {
	case st == nil || st.PID <= 0:
	case !r.alive(st.PID) || !r.sameProcess(st.Identity, st.PID):
		r.clearState()
		s.Reason = "stale_state"
	default:
		s.Running, s.Reason = true, "running"
		s.PID, s.Host, s.Port, s.StartedAt, s.Command = st.PID, st.Host, st.Port, st.StartedAt, st.Command
		if st.LaunchMode == LaunchForeground || st.LaunchMode == LaunchBackground {
			s.LaunchMode = st.LaunchMode
		}
		return s
	}
	if reason != "" {
		s.Reason = reason
	}
	return s
}

// childArgv is the background gateway's command line: this binary's
// foreground `gateway`, detached by the runtime.
func (r *Runtime) childArgv(opts StartOptions) ([]string, error) {
	exe := os.Executable
	if r.Executable != nil {
		exe = r.Executable
	}
	bin, err := exe()
	if err != nil {
		return nil, fmt.Errorf("find the nanobot binary: %w", err)
	}
	argv := []string{bin, "gateway", "--config", r.Instance.ConfigPath}
	if r.Instance.Workspace != "" {
		argv = append(argv, "--workspace", r.Instance.Workspace)
	}
	if opts.Host != "" {
		argv = append(argv, "--host", opts.Host)
	}
	if opts.Port > 0 {
		argv = append(argv, "--port", strconv.Itoa(opts.Port))
	}
	if opts.Verbose {
		argv = append(argv, "--verbose")
	}
	return argv, nil
}

func (r *Runtime) spawn(argv []string, log *os.File) (int, error) {
	if r.Spawn != nil {
		return r.Spawn(argv, log)
	}
	newCmd := exec.Command
	if r.Command != nil {
		newCmd = r.Command
	}
	cmd := newCmd(argv[0], argv[1:]...)
	cmd.Stdin = nil // /dev/null
	cmd.Stdout = log
	cmd.Stderr = log
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// Reap it if it exits while this process runs, so it does not linger
	// as a zombie that still answers kill(pid, 0).
	go cmd.Wait()
	return cmd.Process.Pid, nil
}

// StartBackground starts the gateway detached from the terminal and
// marks it explicit: it keeps running after every client lease is
// released. When it already runs, the result is MsgAlreadyRunning, with
// Promoted set if it was on demand until now.
func (r *Runtime) StartBackground(opts StartOptions) (Result, error) {
	p := r.Paths()
	return locked([]string{p.transitionLock(), p.stateLock()}, func() (Result, error) {
		c := r.liveClients()
		promoted := c.AutoStop
		c.AutoStop = false
		if err := r.writeClients(c); err != nil {
			return Result{}, err
		}
		res, err := r.startLocked(opts)
		res.Promoted = promoted && res.Message == MsgAlreadyRunning
		return res, err
	})
}

// StartOnDemand starts the gateway for the client leases, which stop it
// when the last one is released, unless it already runs.
func (r *Runtime) StartOnDemand(opts StartOptions) (Result, error) {
	p := r.Paths()
	return locked([]string{p.transitionLock(), p.stateLock()}, func() (Result, error) {
		if s := r.statusLocked(""); s.Running {
			return Result{Message: MsgAlreadyRunning, Status: s}, nil
		}
		c := r.liveClients()
		c.AutoStop = true
		if err := r.writeClients(c); err != nil {
			return Result{}, err
		}
		return r.startLocked(opts)
	})
}

// startLocked starts the gateway with both locks held. The state file
// is written before the locks are released, so the child's Claim sees
// itself recorded as the background gateway.
func (r *Runtime) startLocked(opts StartOptions) (Result, error) {
	if s := r.statusLocked(""); s.Running {
		return Result{Message: MsgAlreadyRunning, Status: s}, nil
	}
	argv, err := r.childArgv(opts)
	if err != nil {
		return Result{}, err
	}
	p := r.Paths()
	for _, d := range []string{p.RunDir, p.LogsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return Result{}, err
		}
	}
	log, err := os.OpenFile(p.LogPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return Result{}, err
	}
	now := r.now().UTC()
	fmt.Fprintf(log, "--- %s nanobot gateway starting in the background: %s\n", now.Format(time.RFC3339), strings.Join(argv, " "))
	pid, err := r.spawn(argv, log)
	log.Close()
	if err != nil {
		return Result{}, fmt.Errorf("start the gateway: %w", err)
	}
	st := &State{
		PID:        pid,
		StartedAt:  now.Format(time.RFC3339),
		Platform:   runtime.GOOS,
		Host:       opts.Host,
		Port:       opts.Port,
		ConfigPath: r.Instance.ConfigPath,
		Workspace:  r.Instance.Workspace,
		Command:    argv,
		LogPath:    p.LogPath,
		LaunchMode: LaunchBackground,
		Identity:   r.identity(pid),
	}
	if err := r.writeState(st); err != nil {
		r.terminate(pid, true)
		return Result{}, err
	}
	grace := r.StartupGrace
	if grace == 0 {
		grace = defaultStartupGrace
	}
	r.sleep(grace)
	if !r.alive(pid) {
		if cur := r.readState(); cur != nil && cur.PID == pid {
			r.clearState()
		}
		return Result{Message: MsgExitedDuringStartup, Status: r.statusLocked("exited_during_startup")}, nil
	}
	return Result{OK: true, Message: MsgStarted, Status: r.statusLocked("")}, nil
}

// Stop stops the instance's gateway (SIGTERM, then SIGKILL after
// timeout) and forgets its client leases.
func (r *Runtime) Stop(timeout time.Duration) (Result, error) {
	p := r.Paths()
	return locked([]string{p.transitionLock()}, func() (Result, error) {
		res, err := r.stopTransition(timeout)
		if err == nil && (res.OK || res.Message == MsgNotRunning || res.Message == MsgStateStale) {
			_, err = locked([]string{p.stateLock()}, func() (struct{}, error) {
				os.Remove(p.ClientsPath)
				res.Status = r.statusLocked(res.Status.Reason)
				return struct{}{}, nil
			})
		}
		return res, err
	})
}

// stopTransition stops the gateway holding only the transition lock,
// so the exiting gateway can take the state lock to clear its record.
func (r *Runtime) stopTransition(timeout time.Duration) (Result, error) {
	p := r.Paths()
	s, err := locked([]string{p.stateLock()}, func() (Status, error) { return r.statusLocked(""), nil })
	if err != nil {
		return Result{}, err
	}
	if !s.Running {
		msg := MsgNotRunning
		if s.Reason == "stale_state" {
			msg = MsgStateStale
		}
		return Result{Message: msg, Status: s}, nil
	}
	pid := s.PID
	if err := r.terminate(pid, false); err != nil {
		return Result{Message: MsgStopTimeout, Status: s}, fmt.Errorf("stop gateway pid %d: %w", pid, err)
	}
	if !r.waitExit(pid, timeout) {
		r.terminate(pid, true)
		if !r.waitExit(pid, killWait) {
			return Result{Message: MsgStopTimeout, Status: s}, nil
		}
	}
	return locked([]string{p.stateLock()}, func() (Result, error) {
		if cur := r.readState(); cur != nil && cur.PID == pid {
			r.clearState()
		}
		return Result{OK: true, Message: MsgStopped, Status: r.statusLocked("stopped")}, nil
	})
}

func (r *Runtime) waitExit(pid int, timeout time.Duration) bool {
	deadline := r.now().Add(timeout)
	for r.now().Before(deadline) {
		if !r.alive(pid) {
			return true
		}
		r.sleep(100 * time.Millisecond)
	}
	return !r.alive(pid)
}

// Restart stops a running background gateway and starts it again with
// opts, keeping its lifetime. A foreground gateway is not restarted
// (MsgForegroundRestartRequired): it would come back detached.
func (r *Runtime) Restart(opts StartOptions, timeout time.Duration) (Result, error) {
	p := r.Paths()
	return locked([]string{p.transitionLock()}, func() (Result, error) {
		s, err := locked([]string{p.stateLock()}, func() (Status, error) { return r.statusLocked(""), nil })
		if err != nil {
			return Result{}, err
		}
		if !s.Running {
			return Result{Message: MsgNotRunning, Status: s}, nil
		}
		if s.LaunchMode == LaunchForeground {
			return Result{Message: MsgForegroundRestartRequired, Status: s}, nil
		}
		if res, err := r.stopTransition(timeout); err != nil || !res.OK {
			return res, err
		}
		return locked([]string{p.stateLock()}, func() (Result, error) { return r.startLocked(opts) })
	})
}

// Claim records the calling process as the instance's gateway: a
// foreground `nanobot gateway`, or the background child confirming the
// record its launcher wrote (it keeps launch mode background). It fails
// with *AlreadyRunningError when another live process holds the
// instance. A foreground gateway is explicit: client leases never stop
// it. release clears the record; call it when the gateway exits.
func (r *Runtime) Claim(opts StartOptions) (release func(), err error) {
	p := r.Paths()
	pid := r.getpid()
	_, err = locked([]string{p.stateLock()}, func() (struct{}, error) {
		s := r.statusLocked("")
		if s.Running && s.PID != pid {
			return struct{}{}, &AlreadyRunningError{Status: s}
		}
		prev := r.readState()
		st := &State{
			PID:        pid,
			StartedAt:  r.now().UTC().Format(time.RFC3339),
			Platform:   runtime.GOOS,
			Host:       opts.Host,
			Port:       opts.Port,
			ConfigPath: r.Instance.ConfigPath,
			Workspace:  r.Instance.Workspace,
			Command:    os.Args,
			LogPath:    p.LogPath,
			LaunchMode: LaunchForeground,
			Identity:   r.identity(pid),
		}
		if prev != nil && prev.PID == pid {
			st.StartedAt, st.Command = prev.StartedAt, prev.Command
			if prev.LaunchMode == LaunchBackground {
				st.LaunchMode = LaunchBackground
			}
		}
		if err := r.writeState(st); err != nil {
			return struct{}{}, err
		}
		if st.LaunchMode == LaunchForeground {
			c := r.liveClients()
			c.AutoStop = false
			return struct{}{}, r.writeClients(c)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	return func() {
		locked([]string{p.stateLock()}, func() (struct{}, error) {
			if cur := r.readState(); cur != nil && cur.PID == pid {
				r.clearState()
			}
			c := r.liveClients()
			if len(c.Clients) == 0 {
				os.Remove(p.ClientsPath)
			} else {
				r.writeClients(c)
			}
			return struct{}{}, nil
		})
	}, nil
}

// ErrExited is WaitReady's error when the gateway exits first.
var ErrExited = errors.New("the gateway exited")

// WaitReady polls ready every 100ms until it reports true, failing with
// ErrExited when pid exits first or with a timeout error.
func (r *Runtime) WaitReady(ctx context.Context, pid int, timeout time.Duration, ready func() bool) error {
	deadline := r.now().Add(timeout)
	for {
		if ready() {
			return nil
		}
		if pid > 0 && !r.alive(pid) {
			return ErrExited
		}
		if !r.now().Before(deadline) {
			return fmt.Errorf("the gateway did not become ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// LogTail returns up to n last lines of the gateway log.
func (r *Runtime) LogTail(n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(r.Paths().LogPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := max(info.Size()-logTailBytes, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, off, info.Size()-off))
	if err != nil {
		return nil, err
	}
	if off > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
