package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/gatewayruntime"
	"github.com/yedamao/nanobot-go/internal/gatewayservice"
)

const gatewayServiceUsage = `Usage:
  nanobot gateway service install [--config path] [--workspace dir] [--host addr] [--port n] [--verbose]
                                  [--name name] [--manager auto|launchd|systemd]
                                  [--no-enable] [--no-start] [--dry-run]
  nanobot gateway service uninstall [--name name] [--manager m] [--dry-run]
  nanobot gateway service start [--name name] [--manager m] [--dry-run]
  nanobot gateway service stop [--name name] [--manager m] [--dry-run]
  nanobot gateway service status [--name name] [--manager m] [--config path] [--workspace dir]

'gateway install-service' and 'gateway uninstall-service' are aliases
(nanobot's names).

install writes a macOS LaunchAgent (~/Library/LaunchAgents/<label>.plist,
launchd domain gui/<uid>) or a systemd user unit
(~/.config/systemd/user/<name>.service) that runs the gateway in the
foreground on a fixed port, then loads, enables and starts it. The
service manager restarts the gateway when it fails (launchd: KeepAlive
on a non-zero exit; systemd: Restart=always after 10s), and starts it at
login (launchd RunAtLoad) or boot (systemd, with lingering enabled).
The default name nanobot-go-gateway is the label ai.nanobot-go.gateway and
the unit nanobot-go-gateway.service, apart from nanobot's (Python).

start and status read the config, workspace and port from the installed
plist or unit. install --start and start refuse while another gateway runs for the
config or something listens on the port: stop it first ('nanobot gateway
status', 'nanobot gateway stop').

The service gateway records itself like a foreground one, so 'gateway
status' and 'nanobot webui' see it. Stop it with 'gateway service stop'
(launchd unloads it until the next login or 'service start'; systemd
stops the unit). 'gateway stop' also ends it, but systemd restarts it
after 10 seconds. 'gateway restart' refuses it (foreground); use
'gateway service start' after 'service stop', or reinstall.
`

// serviceInstaller returns the OS service installer.
func (a *app) serviceInstaller() (*gatewayservice.Installer, error) {
	if a.newServiceInstaller != nil {
		return a.newServiceInstaller()
	}
	return gatewayservice.New()
}

// gatewayService runs `gateway service <cmd>`.
func (a *app) gatewayService(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(a.stderr, gatewayServiceUsage)
		return errUsage
	}
	switch args[0] {
	case "install":
		return a.gatewayServiceInstall(ctx, args[1:])
	case "uninstall":
		return a.gatewayServiceUninstall(args[1:])
	case "start":
		return a.gatewayServiceStart(ctx, args[1:])
	case "stop":
		return a.gatewayServiceStop(args[1:])
	case "status":
		return a.gatewayServiceStatus(args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(a.stdout, gatewayServiceUsage)
		return nil
	}
	fmt.Fprintf(a.stderr, "nanobot: unknown gateway service command %q\n\n%s", args[0], gatewayServiceUsage)
	return errUsage
}

// serviceFlags are the --name and --manager flags naming a service.
type serviceFlags struct {
	name, manager string
}

func addServiceFlags(s *gatewayFlags) *serviceFlags {
	f := &serviceFlags{}
	s.fs.StringVar(&f.name, "name", gatewayservice.DefaultName, "service name (launchd label ai.nanobot-go.<name>, systemd unit <name>.service)")
	s.fs.StringVar(&f.manager, "manager", gatewayservice.ManagerAuto, "service manager: auto, launchd or systemd")
	return f
}

func (a *app) gatewayServiceInstall(ctx context.Context, args []string) error {
	s := a.newGatewayFlags("gateway service install", "gateway service install [flags]")
	svc := addServiceFlags(s)
	host := s.fs.String("host", "", "health endpoint host (overrides gateway.host)")
	port := s.fs.Int("port", -1, "health endpoint port (overrides gateway.port; must be fixed, not 0)")
	verbose := s.fs.Bool("verbose", false, "log debug output")
	enable := s.fs.Bool("enable", true, "start the service at login (launchd RunAtLoad) or boot (systemctl enable)")
	noEnable := s.fs.Bool("no-enable", false, "same as --enable=false")
	start := s.fs.Bool("start", true, "start the service now")
	noStart := s.fs.Bool("no-start", false, "same as --start=false")
	dryRun := s.fs.Bool("dry-run", false, "print the service file and commands without installing")
	if err := parse(s.fs, args); err != nil {
		return err
	}
	*enable = *enable && !*noEnable
	*start = *start && !*noStart
	in, err := a.serviceInstaller()
	if err != nil {
		return err
	}
	rt, path, err := s.runtime(a)
	if err != nil {
		return err
	}
	cfg, startOpts, err := gatewayStartConfig(path, *host, *port, *verbose)
	if err != nil {
		return err
	}
	inst := rt.Instance
	opts := gatewayservice.Options{
		Name:       svc.name,
		Manager:    svc.manager,
		Enable:     *enable,
		Start:      *start,
		ConfigPath: inst.ConfigPath,
		Workspace:  inst.Workspace,
		Host:       startOpts.Host,
		Port:       startOpts.Port,
		Verbose:    *verbose,
		LogsDir:    inst.Paths.LogsDir,
	}
	if *start && !*dryRun {
		if err := a.serviceConflict(rt, in, svc, cfg); err != nil {
			return err
		}
	}
	res, err := in.Install(opts, *dryRun)
	if err != nil {
		return fmt.Errorf("gateway service was not installed: %w", err)
	}
	if *dryRun {
		fmt.Fprintln(a.stdout, "Gateway service dry run (nothing was written or run).")
	} else {
		fmt.Fprintln(a.stdout, "Gateway service installed.")
	}
	a.printServiceResult(res, *dryRun)
	if !*start || *dryRun {
		return nil
	}
	return a.waitServiceHealthy(ctx, in, svc, rt, cfg)
}

// serviceConflict refuses to start the service while another gateway
// runs for the config or something answers on its port; the service's
// own gateway (a reinstall) is fine.
func (a *app) serviceConflict(rt *gatewayruntime.Runtime, in *gatewayservice.Installer, svc *serviceFlags, cfg *config.Config) error {
	own, _ := in.Status(svc.name, svc.manager, "")
	st, err := rt.Status()
	if err != nil {
		return err
	}
	if st.Running {
		if own.Loaded && own.PID != 0 && own.PID == st.PID {
			return nil
		}
		a.printGatewayStatus(st)
		return fmt.Errorf("a gateway for this config is already running (pid %d, %s, port %d); stop it with 'nanobot gateway stop' (see 'nanobot gateway status'), or install with --no-start", st.PID, st.LaunchMode, st.Port)
	}
	addr := net.JoinHostPort(browserHost(cfg.Gateway.Host), strconv.Itoa(cfg.Gateway.Port))
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		if own.Loaded && own.PID != 0 {
			// Still starting, or serving before it records itself.
			return nil
		}
		return fmt.Errorf("something already listens on %s (another gateway for a different config, or another program); stop it or pick another --port", addr)
	}
	return nil
}

// waitServiceHealthy waits for the started service's gateway to answer
// /healthz, showing the end of its error log when it does not.
func (a *app) waitServiceHealthy(ctx context.Context, in *gatewayservice.Installer, svc *serviceFlags, rt *gatewayruntime.Runtime, cfg *config.Config) error {
	client := &http.Client{Timeout: time.Second}
	base := localURL(cfg.Gateway.Host, cfg.Gateway.Port, "")
	deadline := time.Now().Add(gatewayReadyTimeout)
	for !gatewayHealthy(ctx, client, base) {
		if time.Now().After(deadline) || ctx.Err() != nil {
			st, _ := in.Status(svc.name, svc.manager, rt.Paths().LogsDir)
			a.printServiceStatus(st)
			a.printFileTail(st.Stderr, 10)
			return fmt.Errorf("the service's gateway did not answer %s/healthz within %s; see its logs listed above and 'nanobot gateway service status'", base, gatewayReadyTimeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Fprintf(a.stdout, "Gateway is serving on %s.\n", base)
	return nil
}

func (a *app) gatewayServiceUninstall(args []string) error {
	s := a.newGatewayFlags("gateway service uninstall", "gateway service uninstall [--name name] [--manager m] [--dry-run]")
	svc := addServiceFlags(s)
	dryRun := s.fs.Bool("dry-run", false, "print the commands without uninstalling")
	if err := parse(s.fs, args); err != nil {
		return err
	}
	in, err := a.serviceInstaller()
	if err != nil {
		return err
	}
	res, err := in.Uninstall(svc.name, svc.manager, *dryRun)
	if err != nil {
		return fmt.Errorf("gateway service was not uninstalled: %w", err)
	}
	if *dryRun {
		fmt.Fprintln(a.stdout, "Gateway service uninstall dry run (nothing was removed or run).")
	} else {
		fmt.Fprintln(a.stdout, "Gateway service uninstalled.")
	}
	a.printServiceResult(res, *dryRun)
	return nil
}

func (a *app) gatewayServiceStart(ctx context.Context, args []string) error {
	s := a.newGatewayFlags("gateway service start", "gateway service start [--name name] [--manager m] [--dry-run]")
	svc := addServiceFlags(s)
	dryRun := s.fs.Bool("dry-run", false, "print the commands without running them")
	if err := parse(s.fs, args); err != nil {
		return err
	}
	in, err := a.serviceInstaller()
	if err != nil {
		return err
	}
	if *dryRun {
		res, err := in.Start(svc.name, svc.manager, true)
		if err == nil {
			a.printServiceResult(res, true)
		}
		return err
	}
	rt, cfg, err := a.serviceGateway(in, svc)
	if err != nil {
		return err
	}
	if err := a.serviceConflict(rt, in, svc, cfg); err != nil {
		return err
	}
	res, err := in.Start(svc.name, svc.manager, false)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, "Gateway service started.")
	a.printServiceResult(res, false)
	return a.waitServiceHealthy(ctx, in, svc, rt, cfg)
}

// serviceGateway returns the runtime and config (with the --host and
// --port of the service applied) of the gateway an installed service
// runs, read from its plist or unit.
func (a *app) serviceGateway(in *gatewayservice.Installer, svc *serviceFlags) (*gatewayruntime.Runtime, *config.Config, error) {
	argv, err := in.InstalledCommand(svc.name, svc.manager)
	if err != nil {
		return nil, nil, err
	}
	fs := flag.NewFlagSet("service gateway", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "", "")
	workspace := fs.String("workspace", "", "")
	host := fs.String("host", "", "")
	port := fs.Int("port", -1, "")
	fs.Bool("verbose", false, "")
	if len(argv) < 2 || argv[1] != "gateway" || fs.Parse(argv[2:]) != nil || *cfgPath == "" {
		return nil, nil, fmt.Errorf("the installed service does not run 'nanobot gateway --config ...': %q; reinstall it", argv)
	}
	rt, err := a.gatewayRuntime(*cfgPath, *workspace)
	if err != nil {
		return nil, nil, err
	}
	cfg, _, err := gatewayStartConfig(*cfgPath, *host, *port, false)
	if err != nil {
		return nil, nil, err
	}
	return rt, cfg, nil
}

func (a *app) gatewayServiceStop(args []string) error {
	s := a.newGatewayFlags("gateway service stop", "gateway service stop [--name name] [--manager m] [--dry-run]")
	svc := addServiceFlags(s)
	dryRun := s.fs.Bool("dry-run", false, "print the commands without running them")
	if err := parse(s.fs, args); err != nil {
		return err
	}
	in, err := a.serviceInstaller()
	if err != nil {
		return err
	}
	res, err := in.Stop(svc.name, svc.manager, *dryRun)
	if err != nil {
		return err
	}
	if !*dryRun {
		fmt.Fprintln(a.stdout, "Gateway service stopped; it starts again at the next login or boot when enabled, or with 'nanobot gateway service start'.")
	}
	a.printServiceResult(res, *dryRun)
	return nil
}

func (a *app) gatewayServiceStatus(args []string) error {
	s := a.newGatewayFlags("gateway service status", "gateway service status [--name name] [--manager m] [--config path] [--workspace dir]")
	s.fs.Lookup("config").Usage = "config file of the gateway to report when the service is not installed"
	s.fs.Lookup("workspace").Usage = "workspace of the gateway to report when the service is not installed"
	svc := addServiceFlags(s)
	if err := parse(s.fs, args); err != nil {
		return err
	}
	in, err := a.serviceInstaller()
	if err != nil {
		return err
	}
	// The gateway the service runs, else the one the flags name.
	rt, _, err := a.serviceGateway(in, svc)
	if errors.Is(err, gatewayservice.ErrNotInstalled) {
		rt, _, err = s.runtime(a)
	}
	if err != nil {
		return err
	}
	st, err := in.Status(svc.name, svc.manager, rt.Paths().LogsDir)
	if err != nil {
		return err
	}
	a.printServiceStatus(st)
	gw, err := rt.Status()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "\nGateway for %s:\n", rt.Instance.ConfigPath)
	a.printGatewayStatus(gw)
	if st.PID != 0 && gw.Running && gw.PID != st.PID {
		fmt.Fprintln(a.stdout, "Note: the running gateway is not the service's process.")
	}
	return nil
}

// printFileTail shows the last n lines of the file at path on stderr.
func (a *app) printFileTail(path string, n int) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return
	}
	lines = lines[max(0, len(lines)-n):]
	fmt.Fprintf(a.stderr, "Last lines of %s:\n", path)
	for _, l := range lines {
		fmt.Fprintf(a.stderr, "  %s\n", l)
	}
}

func (a *app) printServiceResult(res gatewayservice.Result, showContent bool) {
	w := a.stdout
	fmt.Fprintf(w, "Manager:     %s\n", res.Manager)
	fmt.Fprintf(w, "Service:     %s\n", res.Name)
	fmt.Fprintf(w, "Path:        %s\n", res.Path)
	if len(res.Commands) > 0 {
		fmt.Fprintln(w, "Commands:")
		for _, c := range res.Commands {
			fmt.Fprintf(w, "  %s\n", strings.Join(c, " "))
		}
	}
	if showContent && res.Content != "" {
		fmt.Fprintf(w, "\n%s", res.Content)
	}
}

func (a *app) printServiceStatus(st gatewayservice.Status) {
	w := a.stdout
	yes := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	fmt.Fprintf(w, "Manager:     %s\n", st.Manager)
	fmt.Fprintf(w, "Service:     %s\n", st.Name)
	fmt.Fprintf(w, "Path:        %s\n", st.Path)
	fmt.Fprintf(w, "Installed:   %s\n", yes(st.Installed))
	fmt.Fprintf(w, "Loaded:      %s\n", yes(st.Loaded))
	if st.State != "" {
		fmt.Fprintf(w, "State:       %s\n", st.State)
	}
	if st.PID != 0 {
		fmt.Fprintf(w, "PID:         %d\n", st.PID)
	}
	if st.Stdout != "" {
		fmt.Fprintf(w, "Stdout:      %s\n", st.Stdout)
		fmt.Fprintf(w, "Stderr:      %s\n", st.Stderr)
	}
}
