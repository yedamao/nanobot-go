package websocket

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// SkillsPath lists workspace skills as JSON (GET /ui/skills).
	SkillsPath = "/ui/skills"
	// skillsDir is the workspace-relative directory holding skills.
	skillsDir = "skills"
	// skillFile is the markdown file each skill directory must contain.
	skillFile = "SKILL.md"
	// maxSkillDescRunes caps description in the list response.
	maxSkillDescRunes = 280
)

// SkillsList is the body of GET /ui/skills.
type SkillsList struct {
	Skills []SkillView `json:"skills"`
}

// SkillView is one workspace skill without absolute paths or raw body.
type SkillView struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Path is workspace-relative, slash-separated (skills/<name>/SKILL.md).
	Path string `json:"path"`
}

// serveSkills answers GET /ui/skills with skills discovered under
// skills/*/SKILL.md in the workspace.
func (c *Channel) serveSkills(w http.ResponseWriter, _ *http.Request) {
	out := SkillsList{Skills: []SkillView{}}
	skills, err := listWorkspaceSkills(c.workspace.Root)
	if err != nil {
		c.log.Warn("websocket: list skills", "error", err)
		http.Error(w, "cannot list skills", http.StatusInternalServerError)
		return
	}
	out.Skills = skills
	writeJSON(w, out)
}

// listWorkspaceSkills reads skills/<name>/SKILL.md under root through
// os.Root so symlinks cannot leave the workspace.
func listWorkspaceSkills(root string) ([]SkillView, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	entries, err := fs.ReadDir(r.FS(), skillsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []SkillView{}, nil
		}
		return nil, err
	}

	var out []SkillView
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		rel := path.Join(skillsDir, name, skillFile)
		f, err := r.Open(filepath.FromSlash(rel))
		if err != nil {
			continue
		}
		meta, err := readSkillMeta(f)
		f.Close()
		if err != nil {
			continue
		}
		if meta.Name == "" {
			meta.Name = name
		}
		out = append(out, SkillView{
			Name:        meta.Name,
			Description: truncateRunes(meta.Description, maxSkillDescRunes),
			Path:        rel,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

type skillMeta struct {
	Name        string
	Description string
}

// readSkillMeta parses optional YAML frontmatter for name/description,
// else uses the first non-empty body line as description.
func readSkillMeta(r io.Reader) (skillMeta, error) {
	br := bufio.NewReader(r)
	var meta skillMeta

	first, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return meta, err
	}
	if strings.TrimSpace(first) != "---" {
		// No frontmatter: first line (and maybe more) as description.
		line := strings.TrimSpace(first)
		if line == "" {
			line = firstNonEmpty(br)
		}
		meta.Description = stripMarkdownHeading(line)
		return meta, nil
	}

	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" || (err == io.EOF && trimmed == "") {
			if err == io.EOF && trimmed != "---" {
				break
			}
			if meta.Description == "" {
				meta.Description = stripMarkdownHeading(firstNonEmpty(br))
			}
			return meta, nil
		}
		if err != nil && err != io.EOF {
			return meta, err
		}
		key, val, ok := strings.Cut(trimmed, ":")
		if !ok {
			if err == io.EOF {
				break
			}
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		switch key {
		case "name":
			meta.Name = val
		case "description":
			meta.Description = val
		}
		if err == io.EOF {
			break
		}
	}
	if meta.Description == "" {
		meta.Description = stripMarkdownHeading(firstNonEmpty(br))
	}
	return meta, nil
}

func firstNonEmpty(br *bufio.Reader) string {
	for {
		line, err := br.ReadString('\n')
		s := strings.TrimSpace(line)
		if s != "" {
			return s
		}
		if err != nil {
			return ""
		}
	}
}

func stripMarkdownHeading(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "#") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "#"))
	}
	return s
}
