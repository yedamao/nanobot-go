package websocket

import (
	"errors"
	"net/http"

	"github.com/yedamao/nanobot-go/internal/skills"
)

const (
	// SkillsPath lists skills as JSON (GET /ui/skills); PATCH and DELETE
	// /ui/skills/{name} enable/disable and delete one, and POST
	// /ui/skills/install installs a bundled builtin.
	SkillsPath = "/ui/skills"
	// maxSkillBody caps the JSON body of skill mutations.
	maxSkillBody = 1 << 10
)

// SkillsList is the body of GET /ui/skills.
type SkillsList struct {
	Skills []SkillView `json:"skills"`
	// Persistent reports whether enable/disable is saved to the config.
	Persistent bool `json:"persistent"`
}

// SkillView is one skill without absolute paths or its raw body: a
// workspace skill (source "workspace", installed) or a bundled builtin
// not installed yet (source "builtin").
type SkillView = skills.Skill

// skillsManager returns the manager behind /ui/skills: Options.Skills,
// or a read-only one over the workspace.
func (c *Channel) skillsManager() *skills.Manager {
	if c.skills != nil {
		return c.skills
	}
	return skills.NewManager(c.workspace.Root, nil)
}

// serveSkills answers GET /ui/skills.
func (c *Channel) serveSkills(w http.ResponseWriter, _ *http.Request) {
	m := c.skillsManager()
	list, err := m.List()
	if err != nil {
		c.log.Warn("websocket: list skills", "error", err)
		http.Error(w, "cannot list skills", http.StatusInternalServerError)
		return
	}
	writeJSON(w, SkillsList{Skills: list, Persistent: m.Persistent()})
}

type skillEnabledBody struct {
	Enabled *bool `json:"enabled"`
}

// patchSkill answers PATCH /ui/skills/{name} {"enabled":bool}.
func (c *Channel) patchSkill(w http.ResponseWriter, r *http.Request) {
	var body skillEnabledBody
	if err := decodeJSONBody(r, &body, maxSkillBody); err != nil || body.Enabled == nil {
		http.Error(w, `body must be {"enabled":true|false}`, http.StatusBadRequest)
		return
	}
	s, err := c.skills.SetEnabled(r.PathValue("name"), *body.Enabled)
	if err != nil {
		c.skillError(w, "update", err)
		return
	}
	writeJSON(w, s)
}

type skillInstallBody struct {
	Name string `json:"name"`
}

// installSkill answers POST /ui/skills/install {"name":"..."}: copy a
// bundled builtin into skills/<name>/.
func (c *Channel) installSkill(w http.ResponseWriter, r *http.Request) {
	var body skillInstallBody
	if err := decodeJSONBody(r, &body, maxSkillBody); err != nil || body.Name == "" {
		http.Error(w, `body must be {"name":"..."}`, http.StatusBadRequest)
		return
	}
	s, err := c.skills.Install(body.Name)
	if err != nil {
		if errors.Is(err, skills.ErrNotFound) {
			http.Error(w, "no bundled skill of that name", http.StatusBadRequest)
			return
		}
		c.skillError(w, "install", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, s)
}

// deleteSkill answers DELETE /ui/skills/{name}: remove a workspace skill.
func (c *Channel) deleteSkill(w http.ResponseWriter, r *http.Request) {
	if err := c.skills.Delete(r.PathValue("name")); err != nil {
		c.skillError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// skillError answers a failed skill mutation.
func (c *Channel) skillError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, skills.ErrInvalidName):
		http.Error(w, "invalid skill name", http.StatusBadRequest)
	case errors.Is(err, skills.ErrNotFound):
		http.Error(w, "no such skill", http.StatusNotFound)
	case errors.Is(err, skills.ErrExists):
		http.Error(w, "skill already installed", http.StatusConflict)
	case errors.Is(err, skills.ErrBuiltin):
		http.Error(w, "built-in skills cannot be deleted", http.StatusForbidden)
	default:
		c.log.Warn("websocket: "+op+" skill", "error", err)
		http.Error(w, "cannot "+op+" the skill", http.StatusInternalServerError)
	}
}
