package builtin

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/tools"
)

// The runner executes consecutive read-only calls concurrently, so a tool
// that mutates state must never claim to be read-only.
func TestReadOnlyFlags(t *testing.T) {
	r := tools.NewRegistry()
	if err := RegisterAll(r, Config{Workspace: newWS(t)}); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"read_file": true, "list_dir": true, "web_fetch": true,
		"write_file": false, "edit_file": false, "exec": false,
	}
	for name, ro := range want {
		tool, ok := r.Get(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if tool.ReadOnly() != ro {
			t.Errorf("%s.ReadOnly() = %v, want %v", name, tool.ReadOnly(), ro)
		}
		if tool.Description() == "" || tool.Parameters()["type"] != "object" {
			t.Errorf("%s: missing description or object schema", name)
		}
	}
}

func TestRegisterAll(t *testing.T) {
	tests := []struct {
		name string
		cfg  func(Config) Config
		want []string
	}{
		{"all", func(c Config) Config { return c },
			[]string{"edit_file", "exec", "list_dir", "read_file", "web_fetch", "write_file"}},
		{"no exec or web", func(c Config) Config {
			c.Exec.Disabled, c.Web.Disabled = true, true
			return c
		}, []string{"edit_file", "list_dir", "read_file", "write_file"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tools.NewRegistry()
			if err := RegisterAll(r, tt.cfg(Config{Workspace: newWS(t)})); err != nil {
				t.Fatal(err)
			}
			if got := r.Names(); !slices.Equal(got, tt.want) {
				t.Errorf("names = %v, want %v", got, tt.want)
			}
		})
	}

	if err := RegisterAll(tools.NewRegistry(), Config{}); err == nil {
		t.Error("expected error without workspace")
	}
	if err := RegisterAll(tools.NewRegistry(), Config{Workspace: newWS(t), Exec: ExecConfig{AllowPatterns: []string{"["}}}); err == nil {
		t.Error("expected error for bad exec pattern")
	}
}

// TestRegistryRoundTrip drives the tools through the registry the way the
// runner will: write, edit, read back.
func TestRegistryRoundTrip(t *testing.T) {
	r := tools.NewRegistry()
	if err := RegisterAll(r, Config{Workspace: newWS(t), Web: WebFetchConfig{Disabled: true}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	steps := []struct {
		tool, args, want string
	}{
		{"write_file", `{"path":"notes/todo.md","content":"- buy milk\n- call bob\n"}`, "Wrote 22 bytes"},
		{"edit_file", `{"path":"notes/todo.md","old_string":"call bob","new_string":"call alice"}`, "replaced 1"},
		{"read_file", `{"path":"notes/todo.md","limit":"1"}`, "1| - buy milk"},
		{"list_dir", `{"path":"notes"}`, "todo.md"},
		{"exec", `{"command":"grep -c alice notes/todo.md"}`, "1\nExit code: 0"},
	}
	for _, s := range steps {
		res := r.Execute(ctx, s.tool, json.RawMessage(s.args))
		if res.IsError || !strings.Contains(res.Content, s.want) {
			t.Fatalf("%s(%s) = %+v, want %q", s.tool, s.args, res, s.want)
		}
	}
	for _, d := range r.Definitions() {
		if d.Description == "" || d.Parameters["type"] != "object" {
			t.Errorf("bad definition for %s: %+v", d.Name, d)
		}
	}
}
