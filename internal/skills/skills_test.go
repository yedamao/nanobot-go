package skills

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, Dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, File), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(list []Skill) []string {
	var out []string
	for _, s := range list {
		out = append(out, string(s.Source)+":"+s.Name)
	}
	return out
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"cron", "my-skill", "a.b_c", "X1"} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", ".hidden", "-x", "a/b", `a\b`, "a b", "ü", strings.Repeat("a", 65)} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true", bad)
		}
	}
}

func TestParseMeta(t *testing.T) {
	tests := []struct{ in, name, desc string }{
		{"---\nname: Deploy\ndescription: \"Ship it\"\n---\n# Deploy\n", "Deploy", "Ship it"},
		{"---\nname: x\n---\n\n# Heading desc\n", "x", "Heading desc"},
		{"# Plain skill\n\nDo things.\n", "", "Plain skill"},
		{"\n\nhello\n", "", "hello"},
		{"---\ndescription: unterminated", "", "unterminated"},
	}
	for _, tt := range tests {
		m, err := ParseMeta(strings.NewReader(tt.in))
		if err != nil || m.Name != tt.name || m.Description != tt.desc {
			t.Errorf("ParseMeta(%q) = %+v, %v", tt.in, m, err)
		}
	}
}

func TestListMergesBuiltins(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "deploy", "---\nname: Deploy\ndescription: Ship it\n---\n")
	writeSkill(t, root, "weather", "# My weather\n")
	os.MkdirAll(filepath.Join(root, Dir, "empty"), 0o755)
	os.MkdirAll(filepath.Join(root, Dir, ".hidden"), 0o755)
	m := NewManager(root, []string{"deploy", "cron"})
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"workspace:deploy", "workspace:weather", "builtin:cron"}
	if got := names(list); !slices.Equal(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
	deploy, weather, cron := list[0], list[1], list[2]
	if deploy.Enabled || deploy.Builtin || !deploy.Installed || deploy.Path != "skills/deploy/SKILL.md" || deploy.Description != "Ship it" {
		t.Errorf("deploy = %+v", deploy)
	}
	if !weather.Enabled || !weather.Builtin || weather.Description != "My weather" {
		t.Errorf("weather = %+v", weather)
	}
	if cron.Enabled || cron.Installed || cron.Path != "" || cron.Description == "" {
		t.Errorf("cron = %+v", cron)
	}

	empty, err := NewManager(t.TempDir(), nil).List()
	if err != nil || len(empty) == 0 || empty[0].Source != SourceBuiltin {
		t.Errorf("empty workspace list = %+v, %v", empty, err)
	}
}

func TestSetEnabledPersists(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "deploy", "Ship it\n")
	m := NewManager(root, nil)
	var saved [][]string
	m.SetPersist(func(d []string) error { saved = append(saved, d); return nil })

	if s, err := m.SetEnabled("deploy", false); err != nil || s.Enabled {
		t.Fatalf("disable = %+v, %v", s, err)
	}
	if _, err := m.SetEnabled("cron", false); err != nil {
		t.Fatal(err)
	}
	// No change: not saved again.
	if _, err := m.SetEnabled("cron", false); err != nil {
		t.Fatal(err)
	}
	if s, err := m.SetEnabled("deploy", true); err != nil || !s.Enabled {
		t.Fatalf("enable = %+v, %v", s, err)
	}
	want := [][]string{{"deploy"}, {"cron", "deploy"}, {"cron"}}
	if !slices.EqualFunc(saved, want, slices.Equal) {
		t.Errorf("saved = %v, want %v", saved, want)
	}
	if _, err := m.SetEnabled("nope", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown = %v", err)
	}
	if _, err := m.SetEnabled("../x", false); !errors.Is(err, ErrInvalidName) {
		t.Errorf("invalid = %v", err)
	}

	// A failed save leaves the state as it was.
	m.SetPersist(func([]string) error { return errors.New("disk full") })
	if _, err := m.SetEnabled("deploy", false); err == nil {
		t.Fatal("want save error")
	}
	if !m.Enabled("deploy") {
		t.Error("deploy disabled despite failed save")
	}
}

func TestInstallBuiltin(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, nil)
	s, err := m.Install("cron")
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != SourceWorkspace || !s.Builtin || !s.Installed || s.Path != "skills/cron/SKILL.md" {
		t.Errorf("installed = %+v", s)
	}
	data, err := os.ReadFile(filepath.Join(root, "skills", "cron", "SKILL.md"))
	if err != nil || !strings.Contains(string(data), "cron(action=") {
		t.Fatalf("SKILL.md = %q, %v", data, err)
	}
	if _, err := m.Install("cron"); !errors.Is(err, ErrExists) {
		t.Errorf("reinstall = %v", err)
	}
	if _, err := m.Install("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown = %v", err)
	}
	if _, err := m.Install("../cron"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("invalid = %v", err)
	}
	// A workspace dir of a builtin's name is never overwritten.
	writeSkill(t, root, "weather", "mine\n")
	if _, err := m.Install("weather"); !errors.Is(err, ErrExists) {
		t.Errorf("over workspace = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "skills", "weather", "SKILL.md")); string(data) != "mine\n" {
		t.Errorf("weather overwritten: %q", data)
	}
}

func TestDelete(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "deploy", "Ship it\n")
	m := NewManager(root, []string{"deploy", "other"})
	var saved [][]string
	m.SetPersist(func(d []string) error { saved = append(saved, d); return nil })

	if err := m.Delete("cron"); !errors.Is(err, ErrBuiltin) {
		t.Errorf("builtin = %v", err)
	}
	if err := m.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
	if err := m.Delete(".."); !errors.Is(err, ErrInvalidName) {
		t.Errorf("invalid = %v", err)
	}
	if err := m.Delete("deploy"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "skills", "deploy")); !os.IsNotExist(err) {
		t.Errorf("deploy still there: %v", err)
	}
	if want := [][]string{{"other"}}; !slices.EqualFunc(saved, want, slices.Equal) {
		t.Errorf("saved = %v", saved)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "skills"))
	if len(entries) != 0 {
		t.Errorf("leftovers: %v", entries)
	}

	// A failed save puts the skill back.
	writeSkill(t, root, "keep", "x\n")
	m.SetEnabled("keep", false)
	m.SetPersist(func([]string) error { return errors.New("disk full") })
	if err := m.Delete("keep"); err == nil {
		t.Fatal("want save error")
	}
	if _, err := os.Stat(filepath.Join(root, "skills", "keep", "SKILL.md")); err != nil {
		t.Errorf("keep not restored: %v", err)
	}
}

func TestDeleteSymlinkDoesNotFollow(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, File), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, Dir), 0o755)
	if err := os.Symlink(outside, filepath.Join(root, Dir, "link")); err != nil {
		t.Skip(err)
	}
	m := NewManager(root, nil)
	// Outside the workspace: not listed.
	list, _ := m.List()
	for _, s := range list {
		if s.Name == "link" {
			t.Errorf("escaping symlink listed: %+v", s)
		}
	}
	if err := m.Delete("link"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, File)); err != nil {
		t.Errorf("symlink target removed: %v", err)
	}
}

func TestSummary(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, nil)
	if s := m.Summary(); s != "" {
		t.Errorf("empty summary = %q", s)
	}
	writeSkill(t, root, "deploy", "---\ndescription: Ship\n  it\n---\n")
	writeSkill(t, root, "off", "Hidden\n")
	m.SetEnabled("off", false)
	s := m.Summary()
	if !strings.HasPrefix(s, "# Skills") || !strings.Contains(s, "**deploy**: Ship (`skills/deploy/SKILL.md`)") {
		t.Errorf("summary = %q", s)
	}
	if strings.Contains(s, "off") || strings.Contains(s, "cron") || strings.Contains(s, root) {
		t.Errorf("summary lists disabled/builtin/absolute: %q", s)
	}
}
