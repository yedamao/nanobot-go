// Package gatewayservice installs `nanobot gateway` as an OS service: a
// macOS LaunchAgent or a systemd user unit that runs a foreground
// gateway and restarts it when it fails, so it survives logout and
// reboot.
//
// It mirrors nanobot's nanobot/gateway/service.py (GatewayServiceInstaller)
// with names of its own, so both can be installed side by side, plus
// start, stop and status (see docs/PORTING.md).
package gatewayservice

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Managers.
const (
	ManagerAuto    = "auto"
	ManagerLaunchd = "launchd"
	ManagerSystemd = "systemd"
)

// DefaultName is the default service name: the launchd label
// ai.nanobot-go.gateway and the systemd unit nanobot-go-gateway.service.
// nanobot's (Python) are ai.nanobot.gateway and nanobot-gateway.service.
const DefaultName = "nanobot-go-gateway"

// labelPrefix starts every launchd label, keeping them apart from
// nanobot's ai.nanobot.*.
const labelPrefix = "ai.nanobot-go."

// pythonName is nanobot's default service name, refused so that the two
// never share a systemd unit.
const pythonName = "nanobot-gateway"

// Options describe the service to install.
type Options struct {
	// Name is the service name (default DefaultName).
	Name string
	// Manager is auto, launchd or systemd.
	Manager string
	// Enable starts the service at login (RunAtLoad; systemctl enable).
	Enable bool
	// Start (re)starts it now.
	Start bool

	// The gateway's command line: <Executable> gateway --config
	// ConfigPath [--workspace Workspace] [--host Host] --port Port
	// [--verbose]. ConfigPath and Workspace must be absolute; Port must
	// be fixed (positive).
	ConfigPath string
	Workspace  string
	Host       string
	Port       int
	Verbose    bool
	// LogsDir holds the service's stdout and stderr logs (launchd).
	LogsDir string
}

// Result is what an operation did, or would do with dry run.
type Result struct {
	Manager string
	// Name is the launchd label or the systemd unit.
	Name string
	// Path is the plist or unit file.
	Path string
	// Commands are the launchctl or systemctl commands, in order.
	Commands [][]string
	// Content is the plist or unit written (install only).
	Content string
}

// Status is the state of an installed service.
type Status struct {
	Manager   string
	Name      string
	Path      string
	Installed bool // the plist or unit file exists
	Loaded    bool // launchd has it bootstrapped; systemd has it loaded
	// State is launchd's state (running, not running, ...) or systemd's
	// ActiveState (active, inactive, failed, ...); empty when unknown.
	State string
	PID   int
	// Stdout and Stderr are the launchd log files.
	Stdout, Stderr string
}

// Installer renders and installs services. The fields replace the
// environment in tests; zero values use the real one.
type Installer struct {
	// Home is the user's home directory.
	Home string
	// Platform is a GOOS (darwin, linux, ...).
	Platform string
	// UID is the user ID of the launchd domain gui/<uid>.
	UID int
	// Executable is the nanobot binary the service runs.
	Executable func() (string, error)
	// Run runs a launchctl or systemctl command and returns its combined
	// output.
	Run func(name string, args ...string) ([]byte, error)
	// Getenv reads PATH, which the service inherits.
	Getenv func(string) string
}

// New returns an Installer for this user and platform.
func New() (*Installer, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Installer{Home: home, Platform: runtime.GOOS, UID: os.Getuid()}, nil
}

func (in *Installer) run(argv []string) ([]byte, error) {
	if in.Run != nil {
		return in.Run(argv[0], argv[1:]...)
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return out, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
		}
		return out, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, msg)
	}
	return out, nil
}

// mustRun runs argv, failing on an error.
func (in *Installer) mustRun(argv []string) error {
	_, err := in.run(argv)
	return err
}

// tryRun runs argv, ignoring a failure (unloading what is not loaded).
func (in *Installer) tryRun(argv []string) { in.run(argv) }

func (in *Installer) getenv(k string) string {
	if in.Getenv != nil {
		return in.Getenv(k)
	}
	return os.Getenv(k)
}

// ResolveManager maps auto to the platform's manager.
func (in *Installer) ResolveManager(manager string) (string, error) {
	switch manager {
	case "", ManagerAuto:
		switch in.Platform {
		case "darwin":
			return ManagerLaunchd, nil
		case "linux":
			return ManagerSystemd, nil
		}
		return "", fmt.Errorf("no supported service manager on %s (launchd on macOS, systemd on Linux)", in.Platform)
	case ManagerLaunchd, ManagerSystemd:
		return manager, nil
	}
	return "", fmt.Errorf("unknown service manager %q (auto, launchd or systemd)", manager)
}

var unsafeChars = regexp.MustCompile(`[^a-z0-9_.-]+`)

// SafeName is nanobot's _safe_service_name: name lowercased, with runs
// of other characters than [a-z0-9_.-] replaced by "-" and leading and
// trailing dots and dashes trimmed (DefaultName when nothing is left).
func SafeName(name string) string {
	v := unsafeChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	v = strings.Trim(v, ".-")
	if v == "" {
		return DefaultName
	}
	return v
}

// checkName refuses nanobot's default name, whose systemd unit would
// replace nanobot's own.
func checkName(name string) error {
	if s := strings.TrimSuffix(SafeName(name), ".service"); s == pythonName {
		return fmt.Errorf("service name %q is nanobot's (Python) own; pick another (default %s)", name, DefaultName)
	}
	return nil
}

// Label is the launchd label of name: ai.nanobot-go.<rest>, where rest
// is the safe name without a leading "nanobot-go-" and with dashes as
// dots (DefaultName gives ai.nanobot-go.gateway). A name that already
// starts with ai.nanobot-go. is kept.
func Label(name string) string {
	if strings.HasPrefix(name, labelPrefix) && len(name) > len(labelPrefix) {
		return SafeName(name)
	}
	rest := strings.TrimPrefix(SafeName(name), "nanobot-go-")
	return labelPrefix + strings.ReplaceAll(rest, "-", ".")
}

// UnitName is the systemd unit of name: <safe name>.service.
func UnitName(name string) string {
	s := SafeName(name)
	if strings.HasSuffix(s, ".service") {
		return s
	}
	return s + ".service"
}

func (in *Installer) domain() string { return "gui/" + strconv.Itoa(in.UID) }

func (in *Installer) plistPath(label string) string {
	return filepath.Join(in.Home, "Library", "LaunchAgents", label+".plist")
}

func (in *Installer) unitPath(unit string) string {
	return filepath.Join(in.Home, ".config", "systemd", "user", unit)
}

// logPaths are the launchd stdout and stderr files of name in logsDir.
func logPaths(logsDir, name string) (stdout, stderr string) {
	stem := SafeName(name)
	return filepath.Join(logsDir, stem+".launchd.log"), filepath.Join(logsDir, stem+".launchd.err.log")
}

// Command is the service's gateway command line: a foreground gateway
// (never --background: the service manager supervises it).
func (in *Installer) Command(opts Options) ([]string, error) {
	if opts.Port <= 0 || opts.Port > 65535 {
		return nil, fmt.Errorf("a gateway service needs a fixed port, got %d: set gateway.port or pass --port", opts.Port)
	}
	if !filepath.IsAbs(opts.ConfigPath) {
		return nil, fmt.Errorf("config path %q is not absolute", opts.ConfigPath)
	}
	if opts.Workspace != "" && !filepath.IsAbs(opts.Workspace) {
		return nil, fmt.Errorf("workspace %q is not absolute", opts.Workspace)
	}
	exe := os.Executable
	if in.Executable != nil {
		exe = in.Executable
	}
	bin, err := exe()
	if err != nil {
		return nil, fmt.Errorf("find the nanobot binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	// `go run` binaries are deleted on exit.
	if strings.Contains(bin, string(filepath.Separator)+"go-build") {
		return nil, fmt.Errorf("%s is a temporary `go run` binary; build or install nanobot and run install from that binary", bin)
	}
	argv := []string{bin, "gateway", "--config", opts.ConfigPath}
	if opts.Workspace != "" {
		argv = append(argv, "--workspace", opts.Workspace)
	}
	if opts.Host != "" {
		argv = append(argv, "--host", opts.Host)
	}
	argv = append(argv, "--port", strconv.Itoa(opts.Port))
	if opts.Verbose {
		argv = append(argv, "--verbose")
	}
	return argv, nil
}

// workDir is the service's working directory: the workspace, or home.
func (in *Installer) workDir(opts Options) string {
	if opts.Workspace != "" {
		return opts.Workspace
	}
	return in.Home
}

// Install writes the service file and runs the commands that load,
// enable and start it; with dryRun it only reports them.
func (in *Installer) Install(opts Options, dryRun bool) (Result, error) {
	if opts.Name == "" {
		opts.Name = DefaultName
	}
	if err := checkName(opts.Name); err != nil {
		return Result{}, err
	}
	manager, err := in.ResolveManager(opts.Manager)
	if err != nil {
		return Result{}, err
	}
	argv, err := in.Command(opts)
	if err != nil {
		return Result{}, err
	}
	if manager == ManagerLaunchd {
		return in.installLaunchd(opts, argv, dryRun)
	}
	return in.installSystemd(opts, argv, dryRun)
}

func (in *Installer) installLaunchd(opts Options, argv []string, dryRun bool) (Result, error) {
	if opts.LogsDir == "" {
		return Result{}, errors.New("no logs directory for the service")
	}
	label := Label(opts.Name)
	path := in.plistPath(label)
	stdout, stderr := logPaths(opts.LogsDir, opts.Name)
	content := renderPlist(plist{
		Label:            label,
		ProgramArguments: argv,
		WorkingDirectory: in.workDir(opts),
		RunAtLoad:        opts.Enable,
		Path:             in.getenv("PATH"),
		Stdout:           stdout,
		Stderr:           stderr,
	})
	domain, target := in.domain(), in.domain()+"/"+label
	var cmds [][]string
	if opts.Start {
		cmds = append(cmds, []string{"launchctl", "bootstrap", domain, path})
	}
	if opts.Enable {
		cmds = append(cmds, []string{"launchctl", "enable", target})
	}
	if opts.Start {
		cmds = append(cmds, []string{"launchctl", "kickstart", "-k", target})
	}
	res := Result{Manager: ManagerLaunchd, Name: label, Path: path, Commands: cmds, Content: content}
	if dryRun {
		return res, nil
	}
	for _, d := range []string{in.workDir(opts), filepath.Dir(path), opts.LogsDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return res, err
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return res, err
	}
	if opts.Start {
		// Unload an earlier version of this service so bootstrap reads
		// the new plist.
		in.tryRun([]string{"launchctl", "bootout", target})
	}
	for _, c := range cmds {
		if err := in.mustRun(c); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (in *Installer) installSystemd(opts Options, argv []string, dryRun bool) (Result, error) {
	unit := UnitName(opts.Name)
	path := in.unitPath(unit)
	content := renderUnit(fmt.Sprintf("nanobot-go gateway (%s)", opts.Name), argv, in.workDir(opts), in.getenv("PATH"))
	cmds := [][]string{{"systemctl", "--user", "daemon-reload"}}
	if opts.Enable {
		cmds = append(cmds, []string{"systemctl", "--user", "enable", unit})
	}
	if opts.Start {
		cmds = append(cmds, []string{"systemctl", "--user", "restart", unit})
	}
	res := Result{Manager: ManagerSystemd, Name: unit, Path: path, Commands: cmds, Content: content}
	if dryRun {
		return res, nil
	}
	for _, d := range []string{in.workDir(opts), filepath.Dir(path)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return res, err
		}
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return res, err
	}
	for _, c := range cmds {
		if err := in.mustRun(c); err != nil {
			return res, err
		}
	}
	return res, nil
}

// target is the service's manager, launchd label or systemd unit, and
// file.
func (in *Installer) target(name, manager string) (Result, error) {
	if name == "" {
		name = DefaultName
	}
	if err := checkName(name); err != nil {
		return Result{}, err
	}
	m, err := in.ResolveManager(manager)
	if err != nil {
		return Result{}, err
	}
	if m == ManagerLaunchd {
		label := Label(name)
		return Result{Manager: m, Name: label, Path: in.plistPath(label)}, nil
	}
	unit := UnitName(name)
	return Result{Manager: m, Name: unit, Path: in.unitPath(unit)}, nil
}

// Uninstall stops and unloads the service and removes its file.
func (in *Installer) Uninstall(name, manager string, dryRun bool) (Result, error) {
	res, err := in.target(name, manager)
	if err != nil {
		return res, err
	}
	if res.Manager == ManagerLaunchd {
		target := in.domain() + "/" + res.Name
		res.Commands = [][]string{
			{"launchctl", "bootout", target},
			{"launchctl", "disable", target},
		}
		if dryRun {
			return res, nil
		}
		for _, c := range res.Commands {
			in.tryRun(c)
		}
		return res, removeFile(res.Path)
	}
	res.Commands = [][]string{
		{"systemctl", "--user", "disable", "--now", res.Name},
		{"systemctl", "--user", "daemon-reload"},
	}
	if dryRun {
		return res, nil
	}
	in.tryRun(res.Commands[0])
	if err := removeFile(res.Path); err != nil {
		return res, err
	}
	return res, in.mustRun(res.Commands[1])
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ErrNotInstalled is Start's error when the service file is missing.
var ErrNotInstalled = errors.New("gateway service is not installed")

// Start starts the installed service: launchd loads it (when it is not
// loaded) and kickstarts it, which leaves a running one alone; systemd
// starts the unit.
func (in *Installer) Start(name, manager string, dryRun bool) (Result, error) {
	res, err := in.target(name, manager)
	if err != nil {
		return res, err
	}
	if !dryRun {
		if _, err := os.Stat(res.Path); err != nil {
			return res, fmt.Errorf("%w (%s); run 'nanobot gateway service install'", ErrNotInstalled, res.Path)
		}
	}
	if res.Manager == ManagerLaunchd {
		target := in.domain() + "/" + res.Name
		bootstrap := []string{"launchctl", "bootstrap", in.domain(), res.Path}
		kickstart := []string{"launchctl", "kickstart", target}
		res.Commands = [][]string{bootstrap, kickstart}
		if dryRun {
			return res, nil
		}
		if st, _ := in.Status(name, manager, ""); !st.Loaded {
			if err := in.mustRun(bootstrap); err != nil {
				return res, err
			}
		}
		return res, in.mustRun(kickstart)
	}
	res.Commands = [][]string{{"systemctl", "--user", "start", res.Name}}
	if dryRun {
		return res, nil
	}
	return res, in.mustRun(res.Commands[0])
}

// Stop stops the service until the next Start, login (launchd, with
// RunAtLoad) or boot (systemd, when enabled): launchd unloads it
// (bootout), so KeepAlive does not restart it; systemd stops the unit.
func (in *Installer) Stop(name, manager string, dryRun bool) (Result, error) {
	res, err := in.target(name, manager)
	if err != nil {
		return res, err
	}
	if res.Manager == ManagerLaunchd {
		res.Commands = [][]string{{"launchctl", "bootout", in.domain() + "/" + res.Name}}
		if dryRun {
			return res, nil
		}
		if st, _ := in.Status(name, manager, ""); !st.Loaded {
			return res, nil
		}
		return res, in.mustRun(res.Commands[0])
	}
	res.Commands = [][]string{{"systemctl", "--user", "stop", res.Name}}
	if dryRun {
		return res, nil
	}
	return res, in.mustRun(res.Commands[0])
}

// Status reports whether the service is installed and loaded, and its
// process. logsDir, when set, fills in the launchd log paths.
func (in *Installer) Status(name, manager, logsDir string) (Status, error) {
	res, err := in.target(name, manager)
	if err != nil {
		return Status{}, err
	}
	if name == "" {
		name = DefaultName
	}
	st := Status{Manager: res.Manager, Name: res.Name, Path: res.Path}
	if _, err := os.Stat(res.Path); err == nil {
		st.Installed = true
	}
	if res.Manager == ManagerLaunchd {
		if logsDir != "" {
			st.Stdout, st.Stderr = logPaths(logsDir, name)
		}
		out, err := in.run([]string{"launchctl", "print", in.domain() + "/" + res.Name})
		if err != nil {
			return st, nil // not loaded
		}
		st.Loaded = true
		st.State, st.PID = parseLaunchctlPrint(string(out))
		return st, nil
	}
	out, err := in.run([]string{"systemctl", "--user", "show", res.Name, "--property=LoadState,ActiveState,MainPID"})
	if err != nil {
		return st, nil
	}
	props := parseProperties(string(out))
	st.Loaded = props["LoadState"] == "loaded"
	st.State = props["ActiveState"]
	st.PID, _ = strconv.Atoi(props["MainPID"])
	return st, nil
}

// parseLaunchctlPrint reads the service's top-level "state = ..." and
// "pid = ..." lines from `launchctl print` output.
func parseLaunchctlPrint(out string) (state string, pid int) {
	for _, line := range strings.Split(out, "\n") {
		// Top-level keys are indented by exactly one tab.
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch k {
		case "state":
			if state == "" {
				state = v
			}
		case "pid":
			if pid == 0 {
				pid, _ = strconv.Atoi(v)
			}
		}
	}
	return state, pid
}

// parseProperties reads `systemctl show` Key=Value lines.
func parseProperties(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}

// InstalledCommand reads the gateway command line from the installed
// plist (ProgramArguments) or unit (ExecStart).
func (in *Installer) InstalledCommand(name, manager string) ([]string, error) {
	res, err := in.target(name, manager)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(res.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w (%s); run 'nanobot gateway service install'", ErrNotInstalled, res.Path)
	} else if err != nil {
		return nil, err
	}
	var argv []string
	if res.Manager == ManagerLaunchd {
		argv, err = plistArguments(data)
	} else {
		argv, err = unitExecStart(string(data))
	}
	if err == nil && len(argv) == 0 {
		err = errors.New("no command")
	}
	if err != nil {
		return nil, fmt.Errorf("read the gateway command from %s: %w", res.Path, err)
	}
	return argv, nil
}
