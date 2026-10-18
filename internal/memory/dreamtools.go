package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
	"github.com/yedamao/nanobot-go/internal/tools/builtin"
)

// SkillsDir holds workspace skills (skills/<name>/SKILL.md), which Dream
// may create and edit.
const SkillsDir = "skills"

// DreamWritableFiles returns the slash-separated workspace files Dream may
// write besides those under SkillsDir.
func DreamWritableFiles() []string {
	return []string{"SOUL.md", "USER.md", Dir + "/" + LongTermFile}
}

// dreamWriteRule is appended to the descriptions of the guarded tools.
const dreamWriteRule = " During Dream, only SOUL.md, USER.md, memory/MEMORY.md and files under skills/ may be written."

// DreamTools returns the restricted registry Dream runs with, like
// nanobot's build_dream_tools: read_file and list_dir confined to the
// workspace (whatever tools.restrictToWorkspace says), and write_file and
// edit_file that refuse any target other than DreamWritableFiles and files
// under skills/. There is no exec or web_fetch.
//
// The target is checked after resolving symlinks, so a link planted at an
// allowed name cannot redirect a write to history.jsonl or out of the
// workspace. The tool then resolves the path again, so a symlink swapped
// in between by another process could still redirect the write within the
// workspace; Dream itself has no tool that creates symlinks.
func DreamTools(workspace string) (*tools.Registry, error) {
	ws, err := security.NewWorkspace(workspace, true)
	if err != nil {
		return nil, fmt.Errorf("dream tools: %w", err)
	}
	reg := tools.NewRegistry()
	for _, t := range []tools.Tool{
		builtin.NewReadFile(ws),
		builtin.NewListDir(ws),
		&dreamWriteGuard{Tool: builtin.NewWriteFile(ws), ws: ws},
		&dreamWriteGuard{Tool: builtin.NewEditFile(ws), ws: ws},
	} {
		if err := reg.Register(t); err != nil {
			return nil, fmt.Errorf("dream tools: %w", err)
		}
	}
	return reg, nil
}

// dreamWriteGuard wraps a tool that writes the file named by its "path"
// argument and rejects targets Dream may not write.
type dreamWriteGuard struct {
	tools.Tool
	ws *security.Workspace
}

func (g *dreamWriteGuard) Description() string { return g.Tool.Description() + dreamWriteRule }

func (g *dreamWriteGuard) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	args, err := tools.DecodeArgs[struct {
		Path string `json:"path"`
	}](raw)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if err := dreamWritable(g.ws, args.Path); err != nil {
		return tools.Errorf("%v", err)
	}
	return g.Tool.Execute(ctx, raw)
}

// dreamWritable reports whether path, once resolved, is one of the files
// Dream may write.
func dreamWritable(ws *security.Workspace, path string) error {
	real, err := ws.ResolveWrite(path)
	if err != nil {
		return err
	}
	denied := fmt.Errorf("%s may not be written during Dream; write only SOUL.md, USER.md, memory/MEMORY.md or files under skills/", path)
	if !ws.Contains(real) {
		return denied
	}
	rel := filepath.ToSlash(ws.Rel(real))
	for _, f := range DreamWritableFiles() {
		if rel == f {
			return nil
		}
	}
	if strings.HasPrefix(rel, SkillsDir+"/") {
		return nil
	}
	return denied
}
