// Package skills finds the agent's skills: workspace skills under
// skills/<name>/SKILL.md and the builtins bundled with the binary, which
// can be installed (copied) into the workspace. Like nanobot's
// SkillsLoader, a skill's name is its directory name; the agent sees a
// summary of the enabled workspace skills and reads a SKILL.md with
// read_file when it needs it.
package skills

import (
	"bufio"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// Dir is the workspace-relative directory holding skills.
	Dir = "skills"
	// File is the markdown file each skill directory must contain.
	File = "SKILL.md"
	// maxNameLen caps a skill (directory) name.
	maxNameLen = 64
	// maxDescRunes caps a description in listings and the summary.
	maxDescRunes = 280
	// maxSkillFileBytes caps how much of a SKILL.md is read for its
	// metadata.
	maxSkillFileBytes = 64 << 10
)

// Source tells where a skill comes from.
type Source string

const (
	// SourceWorkspace is a skill under the workspace's skills/ directory.
	SourceWorkspace Source = "workspace"
	// SourceBuiltin is a bundled skill not installed in the workspace.
	SourceBuiltin Source = "builtin"
)

var (
	// ErrInvalidName is returned for a name that is not a safe directory
	// name.
	ErrInvalidName = errors.New("invalid skill name")
	// ErrNotFound is returned for an unknown skill.
	ErrNotFound = errors.New("skill not found")
	// ErrExists is returned when installing over an existing workspace
	// skill directory.
	ErrExists = errors.New("skill already installed")
	// ErrBuiltin is returned when deleting a builtin that is not
	// installed in the workspace.
	ErrBuiltin = errors.New("built-in skills cannot be deleted")
)

//go:embed builtin
var builtinFS embed.FS

const builtinRoot = "builtin"

// Skill is one skill as listed: names and workspace-relative paths only.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      Source `json:"source"`
	// Builtin reports whether a bundled skill of this name exists (for a
	// workspace skill: whether it was, or could have been, installed
	// from one).
	Builtin bool `json:"builtin"`
	// Installed reports whether the skill is in the workspace.
	Installed bool `json:"installed"`
	Enabled   bool `json:"enabled"`
	// Path is the workspace-relative, slash-separated SKILL.md path; empty
	// for a builtin that is not installed.
	Path string `json:"path,omitempty"`
}

// ValidName reports whether name is a safe skill directory name: 1-64
// ASCII letters, digits, '-', '_' or '.', not starting with '.' or '-'.
func ValidName(name string) bool {
	if name == "" || len(name) > maxNameLen || name[0] == '.' || name[0] == '-' {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// Meta is a SKILL.md's frontmatter fields the listing uses.
type Meta struct {
	Name        string
	Description string
}

// Manager lists, installs, deletes and enables skills of one workspace.
// The disabled set is held in memory, so the agent's prompt follows a
// change right away; persist, when set, saves it first (to the config's
// agents.defaults.disabledSkills).
type Manager struct {
	root string

	mu       sync.Mutex
	disabled map[string]bool
	persist  func(disabled []string) error
}

// NewManager returns the manager for the workspace at root with the
// given disabled skill names.
func NewManager(root string, disabled []string) *Manager {
	m := &Manager{root: root, disabled: map[string]bool{}}
	for _, name := range disabled {
		m.disabled[name] = true
	}
	return m
}

// SetPersist sets the function that saves the disabled names, sorted,
// before a change applies. Call it before the manager is shared.
func (m *Manager) SetPersist(persist func(disabled []string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.persist = persist
}

// Persistent reports whether changes are saved.
func (m *Manager) Persistent() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.persist != nil
}

// Enabled reports whether name is not disabled.
func (m *Manager) Enabled(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.disabled[name]
}

// List returns the workspace skills followed by the builtins not
// installed, each sorted by name.
func (m *Manager) List() ([]Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list()
}

func (m *Manager) list() ([]Skill, error) {
	ws, err := workspaceSkills(m.root)
	if err != nil {
		return nil, err
	}
	builtins := builtinSkills()
	out := make([]Skill, 0, len(ws)+len(builtins))
	installed := map[string]bool{}
	for _, s := range ws {
		installed[s.Name] = true
		s.Builtin = slices.ContainsFunc(builtins, func(b Skill) bool { return b.Name == s.Name })
		s.Enabled = !m.disabled[s.Name]
		out = append(out, s)
	}
	for _, b := range builtins {
		if installed[b.Name] {
			continue
		}
		b.Enabled = !m.disabled[b.Name]
		out = append(out, b)
	}
	return out, nil
}

// Get returns the listed skill called name.
func (m *Manager) Get(name string) (Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.get(name)
}

func (m *Manager) get(name string) (Skill, error) {
	if !ValidName(name) {
		return Skill{}, ErrInvalidName
	}
	all, err := m.list()
	if err != nil {
		return Skill{}, err
	}
	for _, s := range all {
		if s.Name == name {
			return s, nil
		}
	}
	return Skill{}, ErrNotFound
}

// SetEnabled enables or disables the skill called name, which must be a
// workspace skill or a builtin.
func (m *Manager) SetEnabled(name string, enabled bool) (Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.get(name)
	if err != nil {
		return Skill{}, err
	}
	if s.Enabled == enabled {
		return s, nil
	}
	next := m.disabledNames()
	if enabled {
		next = slices.DeleteFunc(next, func(n string) bool { return n == name })
	} else {
		next = append(next, name)
		slices.Sort(next)
	}
	if err := m.save(next); err != nil {
		return Skill{}, err
	}
	if enabled {
		delete(m.disabled, name)
	} else {
		m.disabled[name] = true
	}
	s.Enabled = enabled
	return s, nil
}

// Install copies the builtin called name into skills/<name>/ in the
// workspace. It never overwrites: an existing directory of that name
// gives ErrExists.
func (m *Manager) Install(name string) (Skill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !ValidName(name) {
		return Skill{}, ErrInvalidName
	}
	src := path.Join(builtinRoot, name)
	if _, err := fs.Stat(builtinFS, path.Join(src, File)); err != nil {
		return Skill{}, ErrNotFound
	}
	r, err := os.OpenRoot(m.root)
	if err != nil {
		return Skill{}, err
	}
	defer r.Close()
	if err := r.MkdirAll(Dir, 0o700); err != nil {
		return Skill{}, fmt.Errorf("create %s: %w", Dir, err)
	}
	dst := filepath.Join(Dir, name)
	if err := r.Mkdir(dst, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return Skill{}, ErrExists
		}
		return Skill{}, fmt.Errorf("create %s: %w", dst, err)
	}
	if err := copyTree(r, src, dst); err != nil {
		// Leave no half-installed skill behind.
		r.RemoveAll(dst)
		return Skill{}, err
	}
	return m.get(name)
}

// copyTree copies the embedded directory src into dst under r, which
// exists and is empty.
func copyTree(r *os.Root, src, dst string) error {
	return fs.WalkDir(builtinFS, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, src), "/")
		if rel == "" {
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return r.Mkdir(target, 0o700)
		}
		data, err := builtinFS.ReadFile(p)
		if err != nil {
			return err
		}
		f, err := r.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		return werr
	})
}

// Delete removes the workspace skill directory skills/<name>/ and drops
// name from the disabled set. A builtin that is not installed gives
// ErrBuiltin.
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !ValidName(name) {
		return ErrInvalidName
	}
	r, err := os.OpenRoot(m.root)
	if err != nil {
		return err
	}
	defer r.Close()
	target := filepath.Join(Dir, name)
	// Lstat: a symlinked skill directory is removed as the link, never
	// followed.
	if _, err := r.Lstat(target); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, berr := fs.Stat(builtinFS, path.Join(builtinRoot, name, File)); berr == nil {
				return ErrBuiltin
			}
			return ErrNotFound
		}
		return err
	}
	// Move the skill aside first so a failed config save can put it back.
	staged := filepath.Join(Dir, ".nanobot-delete-"+name)
	r.RemoveAll(staged)
	if err := r.Rename(target, staged); err != nil {
		return fmt.Errorf("delete skill: %w", err)
	}
	if m.disabled[name] {
		next := slices.DeleteFunc(m.disabledNames(), func(n string) bool { return n == name })
		if err := m.save(next); err != nil {
			r.Rename(staged, target)
			return err
		}
		delete(m.disabled, name)
	}
	if err := r.RemoveAll(staged); err != nil {
		return fmt.Errorf("delete skill: %w", err)
	}
	return nil
}

func (m *Manager) disabledNames() []string {
	names := make([]string, 0, len(m.disabled))
	for n, off := range m.disabled {
		if off {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

func (m *Manager) save(disabled []string) error {
	if m.persist == nil {
		return nil
	}
	return m.persist(disabled)
}

const summaryHeader = "# Skills\n\n" +
	"The following skills extend your capabilities. To use one, read its SKILL.md with read_file " +
	"(paths are relative to the workspace) and follow it.\n\n"

// Summary returns the system prompt section listing the enabled
// workspace skills, or "" when there are none.
func (m *Manager) Summary() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, err := workspaceSkills(m.root)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, s := range ws {
		if m.disabled[s.Name] {
			continue
		}
		fmt.Fprintf(&b, "- **%s**: %s (`%s`)\n", s.Name, oneLine(s.Description), s.Path)
	}
	if b.Len() == 0 {
		return ""
	}
	return summaryHeader + strings.TrimRight(b.String(), "\n")
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "(no description)"
	}
	return s
}

// workspaceSkills reads skills/<name>/SKILL.md under root through os.Root
// so symlinks cannot leave the workspace. Directories with an invalid
// name or no readable SKILL.md are left out.
func workspaceSkills(root string) ([]Skill, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	entries, err := fs.ReadDir(r.FS(), Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []Skill{}, nil
		}
		return nil, err
	}
	out := []Skill{}
	for _, e := range entries {
		name := e.Name()
		if !ValidName(name) {
			continue
		}
		rel := path.Join(Dir, name, File)
		meta, err := readMetaFile(r.FS(), rel)
		if err != nil {
			continue
		}
		out = append(out, Skill{
			Name:        name,
			Description: truncateRunes(meta.Description, maxDescRunes),
			Source:      SourceWorkspace,
			Installed:   true,
			Path:        rel,
		})
	}
	sortByName(out)
	return out, nil
}

// builtinSkills lists the bundled skills.
func builtinSkills() []Skill {
	entries, err := fs.ReadDir(builtinFS, builtinRoot)
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		meta, err := readMetaFile(builtinFS, path.Join(builtinRoot, e.Name(), File))
		if err != nil {
			continue
		}
		out = append(out, Skill{
			Name:        e.Name(),
			Description: truncateRunes(meta.Description, maxDescRunes),
			Source:      SourceBuiltin,
			Builtin:     true,
		})
	}
	sortByName(out)
	return out
}

func sortByName(s []Skill) {
	sort.Slice(s, func(i, j int) bool { return strings.ToLower(s[i].Name) < strings.ToLower(s[j].Name) })
}

// readMetaFile parses the metadata of the regular file name in fsys.
func readMetaFile(fsys fs.FS, name string) (Meta, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return Meta{}, err
	}
	defer f.Close()
	// Never block on a FIFO the agent left in place of SKILL.md.
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return Meta{}, fmt.Errorf("%s: not a regular file", name)
	}
	return ParseMeta(io.LimitReader(f, maxSkillFileBytes))
}

// ParseMeta parses optional YAML frontmatter for name and description;
// without a description it uses the first non-empty body line.
func ParseMeta(r io.Reader) (Meta, error) {
	br := bufio.NewReader(r)
	var meta Meta

	first, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return meta, err
	}
	if strings.TrimSpace(first) != "---" {
		line := strings.TrimSpace(first)
		if line == "" {
			line = firstNonEmpty(br)
		}
		meta.Description = stripMarkdownHeading(line)
		return meta, nil
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return meta, err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}
		if key, val, ok := strings.Cut(trimmed, ":"); ok {
			val = strings.Trim(strings.TrimSpace(val), `"'`)
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "name":
				meta.Name = val
			case "description":
				meta.Description = val
			}
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
		if s := strings.TrimSpace(line); s != "" {
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

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
