package websocket

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/security"
)

func TestSkillsList(t *testing.T) {
	dir := t.TempDir()
	deploy := filepath.Join(dir, "skills", "deploy")
	if err := os.MkdirAll(deploy, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: Deploy\ndescription: Ship it\n---\n# Deploy\n\nSteps...\n"
	if err := os.WriteFile(filepath.Join(deploy, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "skills", "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, "SKILL.md"), []byte("# Plain skill\n\nDo things.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := security.NewWorkspace(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, true, Options{Host: "127.0.0.1", Workspace: ws})
	client := h.srv.Client()

	resp, err := client.Get(h.srv.URL + SkillsPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	var list SkillsList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Skills) != 2 {
		t.Fatalf("skills = %+v", list.Skills)
	}
	if list.Skills[0].Name != "Deploy" || list.Skills[0].Description != "Ship it" || list.Skills[0].Path != "skills/deploy/SKILL.md" {
		t.Errorf("deploy = %+v", list.Skills[0])
	}
	if list.Skills[1].Name != "plain" || !strings.Contains(list.Skills[1].Description, "Plain") {
		t.Errorf("plain = %+v", list.Skills[1])
	}
	if strings.Contains(string(body), dir) {
		t.Error("leaked absolute path")
	}

	empty, err := security.NewWorkspace(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	h2 := newHarnessWith(t, true, Options{Host: "127.0.0.1", Workspace: empty})
	resp, err = h2.srv.Client().Get(h2.srv.URL + SkillsPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty = %d", resp.StatusCode)
	}
	json.Unmarshal(body, &list)
	if list.Skills == nil || len(list.Skills) != 0 {
		t.Errorf("empty skills = %+v", list.Skills)
	}
}

func TestListWorkspaceSkillsMeta(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "skills", "x"), 0o755)
	os.WriteFile(filepath.Join(dir, "skills", "x", "SKILL.md"), []byte("hello\n"), 0o644)
	got, err := listWorkspaceSkills(dir)
	if err != nil || len(got) != 1 || got[0].Name != "x" || got[0].Description != "hello" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
