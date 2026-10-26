package gatewayservice

import (
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeRun records commands; fail makes the ones starting with a prefix
// fail and print returns output for one.
type fakeRun struct {
	cmds   []string
	fail   map[string]bool
	output map[string]string
}

func (f *fakeRun) run(name string, args ...string) ([]byte, error) {
	c := strings.Join(append([]string{name}, args...), " ")
	f.cmds = append(f.cmds, c)
	for p := range f.fail {
		if strings.HasPrefix(c, p) {
			return nil, errors.New(c + ": exit status 1")
		}
	}
	return []byte(f.output[c]), nil
}

func newInstaller(t *testing.T, platform string) (*Installer, *fakeRun) {
	t.Helper()
	f := &fakeRun{fail: map[string]bool{}, output: map[string]string{}}
	return &Installer{
		Home:       t.TempDir(),
		Platform:   platform,
		UID:        501,
		Executable: func() (string, error) { return "/opt/nanobot/bin/nanobot", nil },
		Run:        f.run,
		Getenv:     func(k string) string { return map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin:/bin"}[k] },
	}, f
}

func options(home string) Options {
	return Options{
		Enable:     true,
		Start:      true,
		ConfigPath: "/cfg/config.json",
		Workspace:  "/ws dir",
		Port:       18791,
		LogsDir:    filepath.Join(home, ".nanobot-go", "logs"),
	}
}

// writable is options with the workspace under home, for installs that
// write.
func writable(home string) Options {
	o := options(home)
	o.Workspace = filepath.Join(home, "ws")
	return o
}

// parsedPlist decodes the subset of plist XML renderPlist writes.
type parsedPlist struct {
	Dict struct {
		Inner []byte `xml:",innerxml"`
	} `xml:"dict"`
}

// plistValues returns the top-level keys of a rendered plist mapped to
// their raw XML values.
func plistValues(t *testing.T, content string) map[string]string {
	t.Helper()
	var p parsedPlist
	if err := xml.Unmarshal([]byte(content), &p); err != nil {
		t.Fatalf("plist is not XML: %v\n%s", err, content)
	}
	d := xml.NewDecoder(strings.NewReader("<d>" + string(p.Dict.Inner) + "</d>"))
	vals := map[string]string{}
	var key string
	depth := 0
	var start int64
	for {
		off := d.InputOffset()
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 {
				start = off
			}
		case xml.EndElement:
			if depth == 2 {
				raw := ("<d>" + string(p.Dict.Inner) + "</d>")[start:d.InputOffset()]
				if tk.Name.Local == "key" {
					key = strings.TrimSuffix(strings.TrimPrefix(raw, "<key>"), "</key>")
				} else {
					vals[key] = raw
				}
			}
			depth--
		}
	}
	return vals
}

func TestInstallLaunchdDryRun(t *testing.T) {
	in, f := newInstaller(t, "darwin")
	opts := options(in.Home)
	opts.Host = "127.0.0.1"
	opts.Verbose = true
	res, err := in.Install(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 0 {
		t.Errorf("dry run ran %v", f.cmds)
	}
	if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
		t.Errorf("dry run wrote %s", res.Path)
	}
	if res.Manager != ManagerLaunchd || res.Name != "ai.nanobot-go.gateway" {
		t.Errorf("manager %q label %q", res.Manager, res.Name)
	}
	if res.Name == "ai.nanobot.gateway" {
		t.Error("default label is nanobot's (Python)")
	}
	if want := filepath.Join(in.Home, "Library", "LaunchAgents", "ai.nanobot-go.gateway.plist"); res.Path != want {
		t.Errorf("path %s, want %s", res.Path, want)
	}
	v := plistValues(t, res.Content)
	logs := filepath.Join(in.Home, ".nanobot-go", "logs")
	want := map[string]string{
		"Label": "<string>ai.nanobot-go.gateway</string>",
		"ProgramArguments": "<array>\n\t\t<string>/opt/nanobot/bin/nanobot</string>\n\t\t<string>gateway</string>\n" +
			"\t\t<string>--config</string>\n\t\t<string>/cfg/config.json</string>\n" +
			"\t\t<string>--workspace</string>\n\t\t<string>/ws dir</string>\n" +
			"\t\t<string>--host</string>\n\t\t<string>127.0.0.1</string>\n" +
			"\t\t<string>--port</string>\n\t\t<string>18791</string>\n\t\t<string>--verbose</string>\n\t</array>",
		"WorkingDirectory":     "<string>/ws dir</string>",
		"RunAtLoad":            "<true/>",
		"KeepAlive":            "<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>",
		"EnvironmentVariables": "<dict>\n\t\t<key>PATH</key>\n\t\t<string>/opt/homebrew/bin:/usr/bin:/bin</string>\n\t</dict>",
		"StandardOutPath":      "<string>" + filepath.Join(logs, "nanobot-go-gateway.launchd.log") + "</string>",
		"StandardErrorPath":    "<string>" + filepath.Join(logs, "nanobot-go-gateway.launchd.err.log") + "</string>",
	}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("plist values:\n%#v\nwant\n%#v", v, want)
	}
	if strings.Contains(res.Content, "--background") {
		t.Error("service runs a --background gateway")
	}
	wantCmds := [][]string{
		{"launchctl", "bootstrap", "gui/501", res.Path},
		{"launchctl", "enable", "gui/501/ai.nanobot-go.gateway"},
		{"launchctl", "kickstart", "-k", "gui/501/ai.nanobot-go.gateway"},
	}
	if !reflect.DeepEqual(res.Commands, wantCmds) {
		t.Errorf("commands %v, want %v", res.Commands, wantCmds)
	}
}

func TestInstallLaunchdNoEnableNoStart(t *testing.T) {
	in, _ := newInstaller(t, "darwin")
	opts := options(in.Home)
	opts.Enable, opts.Start = false, false
	res, err := in.Install(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Commands) != 0 {
		t.Errorf("commands %v", res.Commands)
	}
	if v := plistValues(t, res.Content); v["RunAtLoad"] != "<false/>" {
		t.Errorf("RunAtLoad %s", v["RunAtLoad"])
	}
	opts.Enable = true
	res, _ = in.Install(opts, true)
	if want := [][]string{{"launchctl", "enable", "gui/501/ai.nanobot-go.gateway"}}; !reflect.DeepEqual(res.Commands, want) {
		t.Errorf("enable-only commands %v", res.Commands)
	}
}

func TestInstallLaunchdWrites(t *testing.T) {
	in, f := newInstaller(t, "darwin")
	opts := options(in.Home)
	opts.Workspace = filepath.Join(in.Home, "ws")
	opts.Name = "Work Bot"
	res, err := in.Install(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "ai.nanobot-go.work.bot" {
		t.Errorf("label %s", res.Name)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil || string(data) != res.Content {
		t.Fatalf("plist not written: %v", err)
	}
	for _, d := range []string{opts.Workspace, opts.LogsDir} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("%s not created", d)
		}
	}
	target := "gui/501/ai.nanobot-go.work.bot"
	want := []string{
		"launchctl bootout " + target,
		"launchctl bootstrap gui/501 " + res.Path,
		"launchctl enable " + target,
		"launchctl kickstart -k " + target,
	}
	if !reflect.DeepEqual(f.cmds, want) {
		t.Errorf("ran %v, want %v", f.cmds, want)
	}

	// A failed bootout (not loaded) is fine; a failed bootstrap is not.
	f.cmds = nil
	f.fail["launchctl bootout"] = true
	if _, err := in.Install(opts, false); err != nil {
		t.Errorf("bootout failure: %v", err)
	}
	f.fail["launchctl bootstrap"] = true
	if _, err := in.Install(opts, false); err == nil || !strings.Contains(err.Error(), "launchctl bootstrap") {
		t.Errorf("bootstrap failure: %v", err)
	}
}

func TestInstallSystemdDryRun(t *testing.T) {
	in, f := newInstaller(t, "linux")
	opts := options(in.Home)
	res, err := in.Install(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.cmds) != 0 {
		t.Errorf("dry run ran %v", f.cmds)
	}
	if res.Manager != ManagerSystemd || res.Name != "nanobot-go-gateway.service" {
		t.Errorf("manager %q unit %q", res.Manager, res.Name)
	}
	if want := filepath.Join(in.Home, ".config", "systemd", "user", "nanobot-go-gateway.service"); res.Path != want {
		t.Errorf("path %s", res.Path)
	}
	want := `[Unit]
Description=nanobot-go gateway (nanobot-go-gateway)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=/ws dir
ExecStart=/opt/nanobot/bin/nanobot gateway --config /cfg/config.json --workspace "/ws dir" --port 18791
Restart=always
RestartSec=10
Environment=PATH=/opt/homebrew/bin:/usr/bin:/bin
NoNewPrivileges=yes

[Install]
WantedBy=default.target
`
	if res.Content != want {
		t.Errorf("unit:\n%s\nwant\n%s", res.Content, want)
	}
	wantCmds := [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "nanobot-go-gateway.service"},
		{"systemctl", "--user", "restart", "nanobot-go-gateway.service"},
	}
	if !reflect.DeepEqual(res.Commands, wantCmds) {
		t.Errorf("commands %v", res.Commands)
	}
}

func TestInstallSystemdWrites(t *testing.T) {
	in, f := newInstaller(t, "linux")
	opts := options(in.Home)
	opts.Workspace = ""
	opts.Start = false
	res, err := in.Install(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(res.Path); string(data) != res.Content || !strings.Contains(res.Content, "WorkingDirectory="+in.Home+"\n") {
		t.Errorf("unit not written with home as working directory:\n%s", data)
	}
	want := []string{"systemctl --user daemon-reload", "systemctl --user enable nanobot-go-gateway.service"}
	if !reflect.DeepEqual(f.cmds, want) {
		t.Errorf("ran %v", f.cmds)
	}
}

func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/bin/nanobot": "/usr/bin/nanobot",
		"a b":              `"a b"`,
		`a"b\c`:            `"a\"b\\c"`,
		"50%$x":            `"50%%$$x"`,
		"":                 `""`,
	} {
		if got := systemdQuote(in, true); got != want {
			t.Errorf("systemdQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestUninstallDryRun(t *testing.T) {
	in, f := newInstaller(t, "darwin")
	res, err := in.Uninstall("", ManagerAuto, true)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"launchctl", "bootout", "gui/501/ai.nanobot-go.gateway"},
		{"launchctl", "disable", "gui/501/ai.nanobot-go.gateway"},
	}
	if !reflect.DeepEqual(res.Commands, want) || len(f.cmds) != 0 {
		t.Errorf("commands %v, ran %v", res.Commands, f.cmds)
	}

	in, _ = newInstaller(t, "linux")
	res, err = in.Uninstall("nanobot-go-gateway", ManagerAuto, true)
	if err != nil {
		t.Fatal(err)
	}
	want = [][]string{
		{"systemctl", "--user", "disable", "--now", "nanobot-go-gateway.service"},
		{"systemctl", "--user", "daemon-reload"},
	}
	if !reflect.DeepEqual(res.Commands, want) {
		t.Errorf("commands %v", res.Commands)
	}
}

func TestUninstallRemoves(t *testing.T) {
	in, f := newInstaller(t, "darwin")
	res, err := in.Install(writable(in.Home), false)
	if err != nil {
		t.Fatal(err)
	}
	f.cmds = nil
	f.fail["launchctl"] = true // not loaded: best effort
	if _, err := in.Uninstall("", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
		t.Error("plist left behind")
	}
	if len(f.cmds) != 2 {
		t.Errorf("ran %v", f.cmds)
	}
	// Again: nothing to remove.
	if _, err := in.Uninstall("", "", false); err != nil {
		t.Errorf("second uninstall: %v", err)
	}
}

func TestPortRequired(t *testing.T) {
	in, _ := newInstaller(t, "darwin")
	for _, port := range []int{0, -1, 70000} {
		opts := options(in.Home)
		opts.Port = port
		if _, err := in.Install(opts, true); err == nil || !strings.Contains(err.Error(), "fixed port") {
			t.Errorf("port %d: %v", port, err)
		}
	}
}

func TestInstallRejects(t *testing.T) {
	in, _ := newInstaller(t, "darwin")
	for name, mod := range map[string]func(*Options){
		"relative config":    func(o *Options) { o.ConfigPath = "config.json" },
		"relative workspace": func(o *Options) { o.Workspace = "ws" },
		"python name":        func(o *Options) { o.Name = "nanobot-gateway" },
		"python unit":        func(o *Options) { o.Name = "Nanobot Gateway.service" },
		"bad manager":        func(o *Options) { o.Manager = "upstart" },
	} {
		opts := options(in.Home)
		mod(&opts)
		if _, err := in.Install(opts, true); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	in.Executable = func() (string, error) { return "/tmp/go-build123/b001/exe/nanobot", nil }
	if _, err := in.Install(options(in.Home), true); err == nil || !strings.Contains(err.Error(), "go run") {
		t.Errorf("go run binary: %v", err)
	}
	in.Platform = "windows"
	if _, err := in.Install(options(in.Home), true); err == nil {
		t.Error("windows: no error")
	}
}

func TestNames(t *testing.T) {
	for name, want := range map[string][2]string{
		"":                       {"ai.nanobot-go.gateway", "nanobot-go-gateway.service"},
		"nanobot-go-gateway":     {"ai.nanobot-go.gateway", "nanobot-go-gateway.service"},
		"  Work Bot! ":           {"ai.nanobot-go.work.bot", "work-bot.service"},
		"ai.nanobot-go.custom":   {"ai.nanobot-go.custom", "ai.nanobot-go.custom.service"},
		"nanobot-go-test-18799":  {"ai.nanobot-go.test.18799", "nanobot-go-test-18799.service"},
		"..--":                   {"ai.nanobot-go.gateway", "nanobot-go-gateway.service"},
		"bot.service":            {"ai.nanobot-go.bot.service", "bot.service"},
		"ai.nanobot.gateway":     {"ai.nanobot-go.ai.nanobot.gateway", "ai.nanobot.gateway.service"},
		"nanobot-go-gateway-dev": {"ai.nanobot-go.gateway.dev", "nanobot-go-gateway-dev.service"},
	} {
		if got := Label(name); got != want[0] {
			t.Errorf("Label(%q) = %s, want %s", name, got, want[0])
		}
		if got := UnitName(name); got != want[1] {
			t.Errorf("UnitName(%q) = %s, want %s", name, got, want[1])
		}
		if strings.HasPrefix(Label(name), "ai.nanobot.") {
			t.Errorf("Label(%q) is in nanobot's namespace", name)
		}
	}
}

const launchctlPrint = `gui/501/ai.nanobot-go.gateway = {
	active count = 1
	path = /Users/u/Library/LaunchAgents/ai.nanobot-go.gateway.plist
	type = LaunchAgent
	state = running

	program = /opt/nanobot/bin/nanobot
	arguments = {
		/opt/nanobot/bin/nanobot
		gateway
	}

	pid = 4242
	endpoints = {
		state = whatever
	}
}
`

func TestStatus(t *testing.T) {
	in, f := newInstaller(t, "darwin")
	f.fail["launchctl print"] = true
	st, err := in.Status("", "", "/logs")
	if err != nil {
		t.Fatal(err)
	}
	if st.Installed || st.Loaded || st.PID != 0 {
		t.Errorf("not installed: %+v", st)
	}
	if _, err := in.Install(writable(in.Home), false); err != nil {
		t.Fatal(err)
	}
	delete(f.fail, "launchctl print")
	f.output["launchctl print gui/501/ai.nanobot-go.gateway"] = launchctlPrint
	st, _ = in.Status("", "", "/logs")
	want := Status{Manager: "launchd", Name: "ai.nanobot-go.gateway", Path: in.plistPath("ai.nanobot-go.gateway"),
		Installed: true, Loaded: true, State: "running", PID: 4242,
		Stdout: "/logs/nanobot-go-gateway.launchd.log", Stderr: "/logs/nanobot-go-gateway.launchd.err.log"}
	if st != want {
		t.Errorf("status %+v\nwant %+v", st, want)
	}

	in, f = newInstaller(t, "linux")
	f.output["systemctl --user show nanobot-go-gateway.service --property=LoadState,ActiveState,MainPID"] = "LoadState=loaded\nActiveState=active\nMainPID=99\n"
	st, _ = in.Status("", "", "")
	if !st.Loaded || st.State != "active" || st.PID != 99 || st.Installed {
		t.Errorf("systemd status %+v", st)
	}
}

func TestStartStop(t *testing.T) {
	in, f := newInstaller(t, "darwin")
	if _, err := in.Start("", "", false); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("start before install: %v", err)
	}
	opts := writable(in.Home)
	opts.Start = false
	if _, err := in.Install(opts, false); err != nil {
		t.Fatal(err)
	}
	target := "gui/501/ai.nanobot-go.gateway"

	// Not loaded: bootstrap, then kickstart (without -k).
	f.cmds = nil
	f.fail["launchctl print"] = true
	if _, err := in.Start("", "", false); err != nil {
		t.Fatal(err)
	}
	want := []string{"launchctl print " + target, "launchctl bootstrap gui/501 " + in.plistPath("ai.nanobot-go.gateway"), "launchctl kickstart " + target}
	if !reflect.DeepEqual(f.cmds, want) {
		t.Errorf("start ran %v", f.cmds)
	}
	// Stop when not loaded does nothing.
	f.cmds = nil
	if _, err := in.Stop("", "", false); err != nil || len(f.cmds) != 1 {
		t.Errorf("stop unloaded: %v, ran %v", err, f.cmds)
	}

	// Loaded: kickstart only; stop boots it out.
	delete(f.fail, "launchctl print")
	f.cmds = nil
	if _, err := in.Start("", "", false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"launchctl print " + target, "launchctl kickstart " + target}; !reflect.DeepEqual(f.cmds, want) {
		t.Errorf("start loaded ran %v", f.cmds)
	}
	f.cmds = nil
	if _, err := in.Stop("", "", false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"launchctl print " + target, "launchctl bootout " + target}; !reflect.DeepEqual(f.cmds, want) {
		t.Errorf("stop ran %v", f.cmds)
	}

	in, f = newInstaller(t, "linux")
	res, _ := in.Start("", "", true)
	if !reflect.DeepEqual(res.Commands, [][]string{{"systemctl", "--user", "start", "nanobot-go-gateway.service"}}) || len(f.cmds) != 0 {
		t.Errorf("systemd start dry run %v ran %v", res.Commands, f.cmds)
	}
	res, _ = in.Stop("", "", false)
	if !reflect.DeepEqual(f.cmds, []string{"systemctl --user stop nanobot-go-gateway.service"}) {
		t.Errorf("systemd stop ran %v", f.cmds)
	}
}

// TestPlistLint checks the rendered plist with macOS's plutil.
func TestPlistLint(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not available")
	}
	in, _ := newInstaller(t, "darwin")
	opts := options(in.Home)
	opts.Workspace = "/a & <b>"
	res, err := in.Install(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "x.plist")
	os.WriteFile(path, []byte(res.Content), 0o600)
	if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil: %v\n%s", err, out)
	}
	out, err := exec.Command(plutil, "-extract", "WorkingDirectory", "raw", path).Output()
	if err != nil || strings.TrimSpace(string(out)) != "/a & <b>" {
		t.Errorf("WorkingDirectory round trip: %q %v", out, err)
	}
}

// TestInstalledCommand reads back the command line Install wrote.
func TestInstalledCommand(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		in, _ := newInstaller(t, platform)
		if _, err := in.InstalledCommand("", ""); !errors.Is(err, ErrNotInstalled) {
			t.Errorf("%s before install: %v", platform, err)
		}
		opts := writable(in.Home)
		opts.Start = false
		opts.ConfigPath = `/c fg/50% "$x" \y/config.json`
		opts.Host = "0.0.0.0"
		opts.Verbose = true
		want, err := in.Command(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := in.Install(opts, false); err != nil {
			t.Fatal(err)
		}
		got, err := in.InstalledCommand("", "")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %q, %v\nwant %q", platform, got, err, want)
		}
	}
}
