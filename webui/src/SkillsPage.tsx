import { useCallback, useEffect, useState } from "react";
import { deleteSkill, installSkill, listSkills, setSkillEnabled, type Skill, type SkillsList } from "./api";
import { groupSkills, skillBadges } from "./skills";

export function SkillsPage() {
  const [list, setList] = useState<SkillsList | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [note, setNote] = useState("");

  const load = useCallback(async () => {
    setError("");
    try {
      setList(await listSkills());
    } catch (err) {
      setList(null);
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // act runs one change for skill s, then reloads the list.
  const act = async (s: Skill, what: string, change: () => Promise<unknown>) => {
    setBusy(s.name);
    setNote("");
    try {
      await change();
      setNote(what + " " + s.name);
      await load();
    } catch (err) {
      setNote(what + " failed: " + (err as Error).message);
    } finally {
      setBusy(null);
    }
  };

  const toggle = (s: Skill) =>
    act(s, s.enabled ? "Disabled" : "Enabled", () => setSkillEnabled(s.name, !s.enabled));
  const install = (s: Skill) => act(s, "Installed", () => installSkill(s.name));
  const remove = (s: Skill) => {
    if (!confirm('Delete skill "' + s.name + '" and its skills/' + s.name + "/ directory?")) return;
    void act(s, "Deleted", () => deleteSkill(s.name));
  };

  const groups = list ? groupSkills(list.skills) : null;
  const canEdit = !!list?.persistent;

  return (
    <section className="page" aria-labelledby="skills-heading">
      <header className="page-header">
        <h2 id="skills-heading">Skills</h2>
        <button type="button" onClick={() => void load()} disabled={busy !== null}>
          Refresh
        </button>
      </header>
      {error && <p className="page-error">Cannot load skills ({error}).</p>}
      {note && <p className="page-note">{note}</p>}
      {!error && list === null && <p className="page-empty">Loading…</p>}
      {groups && (
        <>
          <h3>Workspace</h3>
          {groups.installed.length === 0 ? (
            <p className="page-empty">
              No workspace skills yet. Install a bundled one below, or let Dream or the agent write{" "}
              <code>skills/&lt;name&gt;/SKILL.md</code>.
            </p>
          ) : (
            <ul className="page-list skills-list">
              {groups.installed.map((s) => (
                <li key={s.name} data-disabled={s.enabled ? undefined : ""}>
                  <div className="skill-head">
                    <strong>{s.name}</strong>
                    {skillBadges(s).map((b) => (
                      <span key={b} className="badge">
                        {b}
                      </span>
                    ))}
                    {canEdit && (
                      <span className="page-actions">
                        <button type="button" disabled={busy !== null} onClick={() => void toggle(s)}>
                          {s.enabled ? "Disable" : "Enable"}
                        </button>
                        <button type="button" disabled={busy !== null} onClick={() => remove(s)}>
                          Delete
                        </button>
                      </span>
                    )}
                  </div>
                  {s.path && <code className="skill-path">{s.path}</code>}
                  {s.description && <p>{s.description}</p>}
                </li>
              ))}
            </ul>
          )}
          {groups.available.length > 0 && (
            <>
              <h3>Bundled</h3>
              <ul className="page-list skills-list">
                {groups.available.map((s) => (
                  <li key={s.name}>
                    <div className="skill-head">
                      <strong>{s.name}</strong>
                      {canEdit && (
                        <span className="page-actions">
                          <button type="button" disabled={busy !== null} onClick={() => void install(s)}>
                            Install
                          </button>
                        </span>
                      )}
                    </div>
                    {s.description && <p>{s.description}</p>}
                  </li>
                ))}
              </ul>
            </>
          )}
          <p className="page-empty">
            Enabled workspace skills are listed in the agent's system prompt from the next turn; the agent reads a
            skill's SKILL.md when it needs it.
          </p>
        </>
      )}
    </section>
  );
}
