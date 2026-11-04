package main

import (
	"runtime"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/security/sandbox"
)

func TestUnavailableSandboxRefusesToStart(t *testing.T) {
	e := newTestEnv(t)
	otherOS := sandbox.Bwrap
	if runtime.GOOS == "linux" {
		otherOS = sandbox.Seatbelt
	}
	cfg := config.Default()
	cfg.Agents.Defaults.Workspace = e.workspace
	cfg.Tools.Exec.Sandbox = otherOS
	cfg.Tools.RestrictToWorkspace = false // a sandbox restricts the file tools anyway
	if err := cfg.Save(e.configPath); err != nil {
		t.Fatal(err)
	}

	if code := e.run(t, nil, "agent", "-m", "hello"); code == 0 {
		t.Fatalf("agent started with an unusable sandbox")
	}
	if !strings.Contains(e.stderr.String(), "refusing to run commands unsandboxed") {
		t.Errorf("stderr = %s", e.stderr)
	}
	if strings.Contains(e.stdout.String(), "echo: hello") {
		t.Errorf("a turn ran: %s", e.stdout)
	}

	if code := e.run(t, nil, "status"); code != 0 {
		t.Fatalf("status exit = %d, stderr %s", code, e.stderr)
	}
	out := e.stdout.String()
	for _, want := range []string{"Exec sandbox:", otherOS + ", unavailable: ", "Restrict to workspace:  true"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}
