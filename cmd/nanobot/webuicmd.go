package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/agent"
	"github.com/yedamao/nanobot-go/internal/channels/websocket"
	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/gatewayruntime"
	"github.com/yedamao/nanobot-go/internal/security"
)

const (
	// webuiReadyTimeout bounds waiting for a gateway this command started
	// to serve the WebUI, like nanobot's 20 seconds.
	webuiReadyTimeout = 20 * time.Second
	// viteReadyTimeout bounds waiting for the Vite dev server.
	viteReadyTimeout = 30 * time.Second
	// defaultViteAddr is where `webui --dev` runs Vite.
	defaultViteAddr = "127.0.0.1:5173"
	// envWebUIDir points `webui --dev` at the webui/ source directory.
	envWebUIDir = "NANOBOT_GO_WEBUI_DIR"
	// envGatewayURL tells webui/vite.config.ts where to proxy.
	envGatewayURL = "NANOBOT_GO_GATEWAY_URL"
)

// webui prepares the local WebUI (enabling the websocket channel in the
// config, with confirmation), then attaches to a gateway already serving
// it on gateway.host:gateway.port or starts one on demand through a
// shared client lease, and opens the browser. It blocks until Ctrl-C or
// SIGTERM, then releases the lease (an on-demand gateway stops when the
// last client exits; a persistent `gateway --background` one does not).
// An unmanaged gateway (no state file) is left alone. With --dev it also
// runs Vite from the source checkout's webui/ with live reload.
//
// A gateway.host beyond loopback needs a sign-in secret; when there is
// none, setup generates channels.websocket.tokenIssueSecret (with
// confirmation) and never prints it.
func (a *app) webui(ctx context.Context, args []string) error {
	fs := a.newFlagSet("webui", "webui [--config path] [--workspace dir] [--host addr] [--port n] [--dev] [--no-open] [--yes]")
	flags := addCommonFlags(fs)
	fs.StringVar(&flags.config, "c", "", "shorthand for --config")
	fs.StringVar(&flags.workspace, "w", "", "shorthand for --workspace")
	host := fs.String("host", "", "gateway host to listen on (sets gateway.host; beyond loopback, browsers must sign in)")
	port := fs.Int("port", 0, "gateway port, where the WebUI is served (sets gateway.port; default from the config)")
	fs.IntVar(port, "p", 0, "shorthand for --port")
	gatewayPort := fs.Int("gateway-port", 0, "same as --port: nanobot-go serves the WebUI on the gateway port")
	dev := fs.Bool("dev", false, "also run the Vite dev server from webui/ with live frontend updates")
	noOpen := fs.Bool("no-open", false, "do not open a browser")
	yes := fs.Bool("yes", false, "apply safe local WebUI defaults without prompting")
	fs.BoolVar(yes, "y", false, "shorthand for --yes")
	background := fs.Bool("background", false, "deprecated: run 'nanobot gateway' yourself, then 'nanobot webui'")
	if err := parse(fs, args); err != nil {
		return err
	}
	path, err := config.ResolvePath(flags.config, a.getenv)
	if err != nil {
		return err
	}
	if *background {
		return backgroundDeprecated(path, flags.workspace)
	}
	p, err := webuiPort(*port, *gatewayPort)
	if err != nil {
		return err
	}

	exists, err := fileExists(path)
	if err != nil {
		return err
	}
	if !exists {
		if err := a.confirm(fmt.Sprintf("No config at %s. Set up nanobot and open WebUI on this device?", path), *yes); err != nil {
			return err
		}
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	changes, secretCreated, err := ensureLocalWebUI(cfg, strings.TrimSpace(*host), p)
	if err != nil {
		return err
	}
	g := cfg.Gateway
	if !security.IsLoopbackHost(g.Host) {
		fmt.Fprintf(a.stderr, "Warning: gateway.host is %q, so the WebUI is reachable beyond this device. "+
			"Browsers there must sign in with channels.websocket.tokenIssueSecret (or token); "+
			"/healthz stays unauthenticated and traffic is plain HTTP. Keep port %d to a trusted network.\n", g.Host, g.Port)
	}
	if len(changes) > 0 && exists {
		fmt.Fprintf(a.stdout, "WebUI setup will update %s: %s.\n", path, strings.Join(changes, ", "))
		if err := a.confirm("Set up WebUI?", *yes); err != nil {
			return err
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if g.Port == 0 {
		return errors.New("gateway.port is 0 (a random port): the WebUI needs a fixed one; pass --port")
	}
	if !exists || len(changes) > 0 {
		if err := cfg.Save(path); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Saved WebUI settings to %s\n", path)
	}
	if secretCreated {
		fmt.Fprintf(a.stdout, "A tokenIssueSecret was written to %s under channels.%s (not shown here); "+
			"open the WebUI from another device and sign in with it.\n", path, websocket.Name)
	}
	if !exists {
		if err := a.writeWorkspace(cfg, flags.workspace); err != nil {
			return err
		}
	}

	base := localURL(g.Host, g.Port, "")
	pageURL := base + "/ui/"
	client := &http.Client{Timeout: time.Second}
	gwrt, err := a.gatewayRuntime(path, flags.workspace)
	if err != nil {
		return err
	}
	st, err := gwrt.Status()
	if err != nil {
		return err
	}
	healthyUI := gatewayHealthy(ctx, client, base) && pageReachable(ctx, client, pageURL)
	managed := st.Running
	var started bool
	switch {
	case healthyUI && !managed:
		// Attach: an unmanaged gateway (older nanobot-go, or another
		// config on this port) that this command never stops.
	case gatewayHealthy(ctx, client, base):
		return errGatewayWithoutWebUI(base, len(changes) > 0)
	case !healthyUI && tcpBusy(net.JoinHostPort(browserHost(g.Host), strconv.Itoa(g.Port))):
		return portConflict(pageURL, base, g.Port)
	default:
		// Share a managed gateway through a client lease (start one on
		// demand when nothing is serving yet).
		lease := gwrt.NewClientLease("webui")
		if err := lease.Acquire(); err != nil {
			return err
		}
		defer a.releaseWebUILease(lease, stopWait(path, 0))
		opts := gatewayruntime.StartOptions{Port: g.Port, Verbose: flags.verbose}
		need := !healthyUI || (managed && len(changes) > 0)
		if need {
			if !healthyUI {
				fmt.Fprintln(a.stdout, "Starting the gateway...")
			}
			started, err = a.ensureWebUIGateway(ctx, gwrt, lease, opts, client, base, pageURL, path, len(changes) > 0)
			if err != nil {
				return err
			}
		}
	}

	openURL := pageURL
	var vite *childProc
	if *dev {
		viteAddr := a.viteAddr
		if viteAddr == "" {
			viteAddr = defaultViteAddr
		}
		openURL = "http://" + viteAddr + "/ui/"
		if vite, err = a.startVite(ctx, client, viteAddr, base, openURL); err != nil {
			return err
		}
		defer a.stopChild(vite, 5*time.Second)
	}

	a.printWebUIReady(openURL, base, path, *dev, started)
	if !*noOpen {
		open := a.openBrowser
		if open == nil {
			open = openBrowser
		}
		if err := open(openURL); err != nil {
			fmt.Fprintln(a.stderr, "Could not open a browser automatically. Open the link above.")
		}
	}
	// Managed gateways are not childProcs of this command; poll health
	// like an attach (lease Release stops an on-demand one on exit).
	return a.waitWebUI(ctx, client, base, gwrt, nil, vite)
}

// errGatewayWithoutWebUI explains a healthy gateway that does not serve /ui/.
func errGatewayWithoutWebUI(base string, configUpdated bool) error {
	msg := fmt.Sprintf("a nanobot-go gateway is already running on %s without the WebUI (channels.websocket is not enabled in it)", base)
	if configUpdated {
		msg += "; the config now enables it"
	}
	return errors.New(msg + ". Stop it with 'nanobot gateway stop' (or from the terminal where you started it) and re-run `nanobot webui`")
}

// ensureWebUIGateway starts or restarts the shared gateway for a webui
// lease and waits until /healthz and /ui/ answer. started is true when
// this call launched a new process (not only attached/restarted).
func (a *app) ensureWebUIGateway(ctx context.Context, rt *gatewayruntime.Runtime, lease *gatewayruntime.ClientLease, opts gatewayruntime.StartOptions, client *http.Client, base, pageURL, path string, changed bool) (started bool, err error) {
	res, err := lease.EnsureOnDemand(opts)
	if err != nil {
		return false, err
	}
	restarted := false
	if !res.OK && res.Message == gatewayruntime.MsgAlreadyRunning && changed {
		fmt.Fprintln(a.stdout, "WebUI config changed; restarting the background gateway.")
		res, err = rt.Restart(opts, stopWait(path, 0))
		restarted = true
		if err != nil {
			return false, err
		}
	}
	if !res.OK && (restarted || res.Message != gatewayruntime.MsgAlreadyRunning) {
		a.printGatewayLogTail(rt)
		action := "started"
		if restarted {
			action = "restarted"
		}
		return false, fmt.Errorf("gateway was not %s: %s (logs: %s)", action, res.Message, rt.Paths().LogPath)
	}
	pid := res.Status.PID
	if err := a.waitGatewayHealthy(ctx, rt, pid, client, base); err != nil {
		return false, err
	}
	if !pageReachable(ctx, client, pageURL) {
		return false, fmt.Errorf("WebUI did not become ready on %s; see %s", pageURL, rt.Paths().LogPath)
	}
	return res.OK && !restarted, nil
}

// releaseWebUILease drops the lease, stopping an on-demand gateway when
// this was the last client.
func (a *app) releaseWebUILease(lease *gatewayruntime.ClientLease, timeout time.Duration) {
	stopped, err := lease.Release(true, timeout)
	if err != nil {
		fmt.Fprintf(a.stderr, "release gateway lease: %v\n", err)
		return
	}
	if stopped {
		fmt.Fprintln(a.stderr, "Stopped the on-demand gateway.")
	}
}

// backgroundDeprecated explains what replaced `webui --background`.
func backgroundDeprecated(path, workspace string) error {
	gw := []string{"nanobot", "gateway", "--background", "--config", path}
	if workspace != "" {
		gw = append(gw, "--workspace", workspace)
	}
	return fmt.Errorf("`nanobot webui --background` no longer owns gateway lifecycle.\n"+
		"Start a persistent gateway, then open the WebUI:\n"+
		"  %s\n  %s",
		shellJoin(gw), shellJoin([]string{"nanobot", "webui", "--config", path}))
}

// webuiPort reconciles --port and --gateway-port (0 when unset). The
// result is 0 when neither is set.
func webuiPort(port, gatewayPort int) (int, error) {
	if port != 0 && gatewayPort != 0 && port != gatewayPort {
		return 0, fmt.Errorf("--port %d and --gateway-port %d differ: nanobot-go serves the WebUI on the gateway port, so there is no separate WebUI port; pass one of them", port, gatewayPort)
	}
	if port == 0 {
		port = gatewayPort
	}
	if port < 0 || port > 65535 {
		return 0, fmt.Errorf("--port must be in [1, 65535], got %d", port)
	}
	return port, nil
}

// ensureLocalWebUI enables the websocket channel (keeping the section's
// other keys) and, when set, sets gateway.host to host and gateway.port
// to port. When gateway.host is then beyond loopback and the section has
// no token or tokenIssueSecret, it generates a tokenIssueSecret and
// reports secretCreated. It reports the changes made to cfg, none when
// it already serves the WebUI there; they never include the secret.
func ensureLocalWebUI(cfg *config.Config, host string, port int) (changes []string, secretCreated bool, err error) {
	raw := cfg.Channels[websocket.Name]
	section := map[string]json.RawMessage{}
	cur := websocket.Config{}
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if cur, err = websocket.ParseConfig(raw); err != nil {
			return nil, false, fmt.Errorf("channels.%s: %w", websocket.Name, err)
		}
		if err := json.Unmarshal(raw, &section); err != nil {
			return nil, false, fmt.Errorf("channels.%s: %w", websocket.Name, err)
		}
	}
	if host != "" && cfg.Gateway.Host != host {
		cfg.Gateway.Host = host
		changes = append(changes, fmt.Sprintf("set gateway.host to %s", host))
	}
	edited := false
	if !cur.Enabled {
		section["enabled"] = json.RawMessage("true")
		edited = true
		changes = append(changes, "enable channels."+websocket.Name)
	}
	if !security.IsLoopbackHost(cfg.Gateway.Host) && !cur.HasSecret() {
		secret, err := json.Marshal(newAccessSecret())
		if err != nil {
			return nil, false, err
		}
		section["tokenIssueSecret"] = secret
		edited, secretCreated = true, true
		changes = append(changes, "generate channels."+websocket.Name+".tokenIssueSecret for sign-in beyond this device")
	}
	if edited {
		data, err := json.Marshal(section)
		if err != nil {
			return nil, false, err
		}
		if cfg.Channels == nil {
			cfg.Channels = map[string]json.RawMessage{}
		}
		cfg.Channels[websocket.Name] = data
	}
	if port > 0 && cfg.Gateway.Port != port {
		cfg.Gateway.Port = port
		changes = append(changes, fmt.Sprintf("set gateway.port to %d", port))
	}
	return changes, secretCreated, nil
}

// newAccessSecret returns 32 random bytes, URL-safe base64 encoded, like
// Python's secrets.token_urlsafe(32).
func newAccessSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// confirm asks question on a terminal; without one (or with yes) it
// fails (or passes) at once.
func (a *app) confirm(question string, yes bool) error {
	if yes {
		return nil
	}
	if !a.canPrompt() {
		return errors.New("WebUI setup needs confirmation: re-run with --yes (-y)")
	}
	fmt.Fprintf(a.stdout, "%s [y/N] ", question)
	line, _ := bufio.NewReader(a.stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errors.New("WebUI setup cancelled")
}

func (a *app) canPrompt() bool {
	if a.isTerminal != nil {
		return a.isTerminal()
	}
	f, ok := a.stdin.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// writeWorkspace creates the workspace and its bootstrap files for a
// config this command just created, as onboard does.
func (a *app) writeWorkspace(cfg *config.Config, override string) error {
	c := *cfg
	if override != "" {
		c.Agents.Defaults.Workspace = override
	}
	ws, err := c.WorkspacePath()
	if err != nil {
		return err
	}
	if _, err := agent.WriteBootstrapFiles(ws); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Workspace: %s\n", ws)
	return nil
}

// browserHost maps a bind host to one a local browser can open.
func browserHost(host string) string {
	h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(host), "["), "]")
	switch h {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::":
		return "::1"
	}
	return h
}

// localURL returns http://host:port+path for a local browser.
func localURL(host string, port int, path string) string {
	return "http://" + net.JoinHostPort(browserHost(host), strconv.Itoa(port)) + path
}

// gatewayHealthy reports whether base serves the gateway's health
// endpoint.
func gatewayHealthy(ctx context.Context, client *http.Client, base string) bool {
	body, ok := httpGet(ctx, client, base+"/healthz")
	if !ok {
		return false
	}
	var h struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(body, &h) == nil && h.Status == "ok"
}

// pageReachable reports whether url answers 200.
func pageReachable(ctx context.Context, client *http.Client, url string) bool {
	_, ok := httpGet(ctx, client, url)
	return ok
}

func httpGet(ctx context.Context, client *http.Client, url string) ([]byte, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	return body, err == nil && resp.StatusCode == http.StatusOK
}

// tcpBusy reports whether something accepts connections on addr.
func tcpBusy(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func portConflict(pageURL, base string, port int) error {
	return fmt.Errorf("cannot open the WebUI: the address is already in use.\n"+
		"  Page:    %s\n  Service: %s/healthz\n"+
		"Something other than a nanobot-go gateway listens on port %d. Stop it, or choose an unused port with --port",
		pageURL, base, port)
}

// waitReady polls until ready reports true, the child exits or timeout
// passes.
func waitReady(ctx context.Context, c *childProc, timeout time.Duration, ready func() bool) error {
	deadline := time.Now().Add(timeout)
	for {
		if ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return fmt.Errorf("%s exited before it was ready (%v); see its output above", c.name, c.err)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become ready within %s; see its output above", c.name, timeout)
		}
	}
}

// startVite runs `npm run dev` in webui/ with the proxy pointed at base
// and waits until it serves openURL.
func (a *app) startVite(ctx context.Context, client *http.Client, viteAddr, base, openURL string) (*childProc, error) {
	dir, err := a.webuiSourceDir()
	if err != nil {
		return nil, err
	}
	lookPath := exec.LookPath
	if a.lookPath != nil {
		lookPath = a.lookPath
	}
	npm, err := lookPath("npm")
	if err != nil {
		return nil, errors.New("--dev needs Node.js and npm on PATH (https://nodejs.org)")
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		return nil, fmt.Errorf("--dev: %s has no node_modules; run `npm install` there first", dir)
	}
	if tcpBusy(viteAddr) {
		return nil, fmt.Errorf("--dev: %s is already in use (another Vite dev server?); stop it first", viteAddr)
	}
	host, vport, err := net.SplitHostPort(viteAddr)
	if err != nil {
		return nil, err
	}
	cmd := a.command(npm, "run", "dev", "--", "--host", host, "--port", vport, "--strictPort")
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), envGatewayURL+"="+base)
	cmd.Stdout = a.stderr
	cmd.Stderr = a.stderr
	vite, err := startChild("Vite dev server", cmd, true)
	if err != nil {
		return nil, err
	}
	if err := waitReady(ctx, vite, viteReadyTimeout, func() bool {
		return pageReachable(ctx, client, openURL)
	}); err != nil {
		a.stopChild(vite, 5*time.Second)
		return nil, err
	}
	return vite, nil
}

// webuiSourceDir finds the webui/ source directory for --dev:
// $NANOBOT_GO_WEBUI_DIR, else the checkout around the working directory
// or the binary (bin/nanobot).
func (a *app) webuiSourceDir() (string, error) {
	if d := a.getenv(envWebUIDir); d != "" {
		if _, err := os.Stat(filepath.Join(d, "package.json")); err != nil {
			return "", fmt.Errorf("--dev: %s=%s has no package.json", envWebUIDir, d)
		}
		return filepath.Abs(d)
	}
	var starts []string
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	for _, d := range starts {
		for {
			if _, err := os.Stat(filepath.Join(d, "webui", "vite.config.ts")); err == nil {
				return filepath.Join(d, "webui"), nil
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	return "", fmt.Errorf("--dev needs the nanobot-go source checkout: run it inside the repository or set %s to its webui/ directory", envWebUIDir)
}

func (a *app) printWebUIReady(openURL, base, path string, dev, started bool) {
	w := a.stdout
	fmt.Fprintln(w)
	if dev {
		fmt.Fprintln(w, "WebUI ready (development)")
	} else {
		fmt.Fprintln(w, "WebUI ready")
	}
	fmt.Fprintf(w, "\nOpen in your browser:\n  %s\n\n", openURL)
	if dev {
		fmt.Fprintf(w, "Gateway: %s/ui/ (Vite proxies to it)\n", base)
	}
	fmt.Fprintf(w, "Health:  %s/healthz\n", base)
	fmt.Fprintf(w, "Config:  %s\n\n", path)
	if u, err := url.Parse(openURL); err == nil && !security.IsLoopbackHost(u.Hostname()) {
		fmt.Fprintf(w, "That address is beyond loopback: sign in with channels.%s.tokenIssueSecret (or token) from the config.\n\n", websocket.Name)
	}
	switch {
	case started:
		fmt.Fprintln(w, "This command started an on-demand gateway; keep this terminal open while using WebUI.")
		fmt.Fprintln(w, "Press Ctrl-C to stop it (unless another local client still holds a lease).")
	case dev:
		fmt.Fprintln(w, "Attached to a gateway that was already running; this command will not stop it.")
		fmt.Fprintln(w, "Press Ctrl-C to stop development mode.")
	default:
		fmt.Fprintln(w, "Attached to a gateway that was already running; to stop a managed one use 'nanobot gateway stop'.")
		fmt.Fprintln(w, "Press Ctrl-C to exit.")
	}
	fmt.Fprintln(w)
}

// waitWebUI blocks until Ctrl-C, SIGTERM, a child exiting or (when
// attached) the gateway going away. A managed gateway that stops
// answering is not gone while rt still records one running once any
// restart in progress (`nanobot gateway restart`, or one the WebUI
// started) has finished. The deferred stopChild calls then stop the
// children.
func (a *app) waitWebUI(ctx context.Context, client *http.Client, base string, rt *gatewayruntime.Runtime, gw, vite *childProc) error {
	var gwDone, viteDone <-chan struct{}
	var poll <-chan time.Time
	if gw != nil {
		gwDone = gw.done
	} else {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		poll = t.C
	}
	if vite != nil {
		viteDone = vite.done
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-a.interrupts:
			fmt.Fprintln(a.stderr, "\nStopping...")
			return nil
		case <-gwDone:
			return fmt.Errorf("the gateway stopped (%v); see its output above", gw.err)
		case <-viteDone:
			return fmt.Errorf("the Vite dev server stopped (%v); see its output above", vite.err)
		case <-poll:
			if !gatewayHealthy(ctx, client, base) && !gatewayRecorded(rt) {
				fmt.Fprintln(a.stderr, "Gateway stopped.")
				return nil
			}
		}
	}
}

// gatewayRecorded reports whether rt records a running gateway once
// any lifecycle operation in progress has finished.
func gatewayRecorded(rt *gatewayruntime.Runtime) bool {
	if rt == nil {
		return false
	}
	st, err := rt.Settled()
	return err == nil && st.Running
}

// childProc is a process this command started and must stop.
type childProc struct {
	name  string
	cmd   *exec.Cmd
	group bool // signal its whole process group
	done  chan struct{}
	err   error // the Wait result, set before done is closed
}

func (a *app) command(name string, args ...string) *exec.Cmd {
	if a.newCommand != nil {
		return a.newCommand(name, args...)
	}
	return exec.Command(name, args...)
}

func startChild(name string, cmd *exec.Cmd, group bool) (*childProc, error) {
	ownProcessGroup(cmd)
	// Do not wait forever for output from grandchildren that outlive it.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the %s: %w", name, err)
	}
	c := &childProc{name: name, cmd: cmd, group: group, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *childProc) signal(sig os.Signal) {
	p := c.cmd.Process
	if c.group {
		if signalGroup(p, sig) == nil {
			return
		}
	}
	if p.Signal(sig) != nil {
		p.Kill()
	}
}

// stopChild interrupts c and waits up to grace for it to exit, then
// kills it. Another Ctrl-C meanwhile is passed on (the gateway then
// exits at once).
func (a *app) stopChild(c *childProc, grace time.Duration) {
	select {
	case <-c.done:
		return
	default:
	}
	fmt.Fprintf(a.stderr, "Stopping the %s...\n", c.name)
	c.signal(os.Interrupt)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-a.interrupts:
			c.signal(os.Interrupt)
		case <-timer.C:
			c.signal(os.Kill)
			<-c.done
			return
		}
	}
}

// logFilter passes on whole lines, dropping the gateway's INFO and DEBUG
// log records unless all is set; warnings, errors and other output
// (such as "nanobot: ..." failures) go through.
type logFilter struct {
	w   io.Writer
	all bool
	buf []byte
}

func (f *logFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			break
		}
		line := f.buf[:i+1]
		if f.all || keepLogLine(line) {
			f.w.Write(line)
		}
		f.buf = f.buf[i+1:]
	}
	return len(p), nil
}

func keepLogLine(line []byte) bool {
	if !bytes.HasPrefix(line, []byte("time=")) {
		return true
	}
	return bytes.Contains(line, []byte(" level=WARN")) || bytes.Contains(line, []byte(" level=ERROR"))
}

// openBrowser opens url in the desktop's browser.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Run()
}

// shellJoin quotes args for a POSIX shell.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, s := range args {
		if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:@%+,") == "" {
			out[i] = s
			continue
		}
		out[i] = "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}
