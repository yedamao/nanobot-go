package websocket

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/skills"
)

func writeTestSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skillsRequest sends a same-origin request and returns the status and
// body.
func skillsRequest(t *testing.T, h *harness, method, path, body string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", h.srv.URL)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func listSkillsHTTP(t *testing.T, h *harness) SkillsList {
	t.Helper()
	code, body := skillsRequest(t, h, http.MethodGet, SkillsPath, "")
	if code != http.StatusOK {
		t.Fatalf("GET = %d %s", code, body)
	}
	var list SkillsList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func skillNames(list SkillsList) []string {
	var out []string
	for _, s := range list.Skills {
		out = append(out, string(s.Source)+":"+s.Name)
	}
	return out
}

func TestSkillsList(t *testing.T) {
	dir := t.TempDir()
	writeTestSkill(t, dir, "deploy", "---\nname: Deploy\ndescription: Ship it\n---\n# Deploy\n\nSteps...\n")
	writeTestSkill(t, dir, "plain", "# Plain skill\n\nDo things.\n")
	ws, err := security.NewWorkspace(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	// Workspace only: read-only listing, no mutations.
	h := newHarnessWith(t, true, Options{Host: "127.0.0.1", Workspace: ws})
	list := listSkillsHTTP(t, h)
	if want := []string{"workspace:deploy", "workspace:plain", "builtin:cron", "builtin:weather"}; !slices.Equal(skillNames(list), want) {
		t.Fatalf("skills = %v, want %v", skillNames(list), want)
	}
	if list.Persistent {
		t.Error("read-only listing reported persistent")
	}
	d := list.Skills[0]
	if d.Description != "Ship it" || d.Path != "skills/deploy/SKILL.md" || !d.Enabled || !d.Installed {
		t.Errorf("deploy = %+v", d)
	}
	if !strings.Contains(list.Skills[1].Description, "Plain") {
		t.Errorf("plain = %+v", list.Skills[1])
	}
	if code, _ := skillsRequest(t, h, http.MethodPatch, SkillsPath+"/deploy", `{"enabled":false}`); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH without Skills = %d", code)
	}
	_, body := skillsRequest(t, h, http.MethodGet, SkillsPath, "")
	if strings.Contains(string(body), dir) {
		t.Error("leaked absolute path")
	}
}

func TestSkillsMutations(t *testing.T) {
	dir := t.TempDir()
	writeTestSkill(t, dir, "deploy", "Ship it\n")
	ws, err := security.NewWorkspace(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	mgr := skills.NewManager(dir, nil)
	var saved [][]string
	mgr.SetPersist(func(d []string) error { saved = append(saved, d); return nil })
	h := newHarnessWith(t, true, Options{Host: "127.0.0.1", Workspace: ws, Skills: mgr})

	// Disable a workspace skill and a builtin.
	code, body := skillsRequest(t, h, http.MethodPatch, SkillsPath+"/deploy", `{"enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("disable = %d %s", code, body)
	}
	var s SkillView
	json.Unmarshal(body, &s)
	if s.Name != "deploy" || s.Enabled {
		t.Errorf("disabled = %+v", s)
	}
	if code, body := skillsRequest(t, h, http.MethodPatch, SkillsPath+"/weather", `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable builtin = %d %s", code, body)
	}
	if strings.Contains(mgr.Summary(), "deploy") {
		t.Error("disabled skill still in summary")
	}
	list := listSkillsHTTP(t, h)
	if !list.Persistent || list.Skills[0].Enabled {
		t.Errorf("list after disable = %+v", list)
	}

	for _, tt := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPatch, "/nope", `{"enabled":true}`, http.StatusNotFound},
		{http.MethodPatch, "/deploy", `{}`, http.StatusBadRequest},
		{http.MethodPatch, "/deploy", `{"enabled":true,"x":1}`, http.StatusBadRequest},
		{http.MethodPatch, "/a%20b", `{"enabled":true}`, http.StatusBadRequest},
		{http.MethodPost, "/install", `{"name":"nope"}`, http.StatusBadRequest},
		{http.MethodPost, "/install", `{"name":"../x"}`, http.StatusBadRequest},
		{http.MethodPost, "/install", `{}`, http.StatusBadRequest},
		{http.MethodDelete, "/cron", "", http.StatusForbidden},
		{http.MethodDelete, "/nope", "", http.StatusNotFound},
	} {
		if code, body := skillsRequest(t, h, tt.method, SkillsPath+tt.path, tt.body); code != tt.want {
			t.Errorf("%s %s %s = %d %s, want %d", tt.method, tt.path, tt.body, code, body, tt.want)
		}
	}

	// Install a builtin, then again.
	code, body = skillsRequest(t, h, http.MethodPost, SkillsPath+"/install", `{"name":"cron"}`)
	if code != http.StatusCreated {
		t.Fatalf("install = %d %s", code, body)
	}
	json.Unmarshal(body, &s)
	if s.Source != skills.SourceWorkspace || s.Path != "skills/cron/SKILL.md" {
		t.Errorf("installed = %+v", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", "cron", "SKILL.md")); err != nil {
		t.Error(err)
	}
	if code, _ := skillsRequest(t, h, http.MethodPost, SkillsPath+"/install", `{"name":"cron"}`); code != http.StatusConflict {
		t.Errorf("reinstall = %d", code)
	}

	// Delete clears the disabled entry.
	if code, body := skillsRequest(t, h, http.MethodDelete, SkillsPath+"/deploy", ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", code, body)
	}
	if want := [][]string{{"deploy"}, {"deploy", "weather"}, {"weather"}}; !slices.EqualFunc(saved, want, slices.Equal) {
		t.Errorf("saved = %v, want %v", saved, want)
	}
	if want := []string{"workspace:cron", "builtin:weather"}; !slices.Equal(skillNames(listSkillsHTTP(t, h)), want) {
		t.Errorf("after delete = %v", skillNames(listSkillsHTTP(t, h)))
	}

	// Cross-origin mutations are refused.
	req, _ := http.NewRequest(http.MethodDelete, h.srv.URL+SkillsPath+"/cron", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin delete = %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", "cron")); err != nil {
		t.Error("cross-origin delete removed the skill")
	}
}
