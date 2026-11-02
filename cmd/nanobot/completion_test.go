package main

import (
	"strings"
	"testing"
)

func TestCompletionUsage(t *testing.T) {
	e := newTestEnv(t)
	if code := e.run(t, nil, "completion"); code != 2 || !strings.Contains(e.stderr.String(), "bash|zsh|fish|powershell") {
		t.Fatalf("completion: code %d, stderr %q", code, e.stderr.String())
	}
	for _, help := range []string{"help", "-h", "--help"} {
		e = newTestEnv(t)
		if code := e.run(t, nil, "completion", help); code != 0 || !strings.Contains(e.stderr.String(), "Usage:") {
			t.Errorf("completion %s: code %d, stderr %q", help, code, e.stderr.String())
		}
	}
}

func TestCompletionScripts(t *testing.T) {
	tests := map[string]string{
		"bash":       "complete -F _nanobot",
		"zsh":        "#compdef nanobot nanobot-go",
		"fish":       "complete -c $cmd",
		"powershell": "Register-ArgumentCompleter",
		"pwsh":       "Register-ArgumentCompleter",
	}
	for shell, marker := range tests {
		t.Run(shell, func(t *testing.T) {
			e := newTestEnv(t)
			if code := e.run(t, nil, "completion", shell); code != 0 || !strings.Contains(e.stdout.String(), marker) {
				t.Fatalf("code %d, stdout %q, stderr %q", code, e.stdout.String(), e.stderr.String())
			}
		})
	}
}

func TestCompletionRejectsUnknownShell(t *testing.T) {
	e := newTestEnv(t)
	if code := e.run(t, nil, "completion", "tcsh"); code != 2 || !strings.Contains(e.stderr.String(), `unknown shell "tcsh"`) {
		t.Fatalf("code %d, stderr %q", code, e.stderr.String())
	}
}

func TestUsageIncludesCompletion(t *testing.T) {
	e := newTestEnv(t)
	if code := e.run(t, nil, "help"); code != 0 || !strings.Contains(e.stdout.String(), "completion") {
		t.Fatalf("code %d, stdout %q", code, e.stdout.String())
	}
}
