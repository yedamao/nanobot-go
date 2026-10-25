import { useCallback, useEffect, useState } from "react";
import { listSkills, type Skill } from "./api";

export function SkillsPage() {
  const [skills, setSkills] = useState<Skill[] | null>(null);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    setError("");
    try {
      setSkills(await listSkills());
    } catch (err) {
      setSkills(null);
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <section className="page" aria-labelledby="skills-heading">
      <header className="page-header">
        <h2 id="skills-heading">Skills</h2>
        <button type="button" onClick={() => void load()}>
          Refresh
        </button>
      </header>
      {error && <p className="page-error">Cannot load skills ({error}).</p>}
      {!error && skills === null && <p className="page-empty">Loading…</p>}
      {!error && skills && skills.length === 0 && (
        <p className="page-empty">
          No workspace skills yet. Dream or the agent can write <code>skills/&lt;name&gt;/SKILL.md</code>.
        </p>
      )}
      {skills && skills.length > 0 && (
        <ul className="page-list skills-list">
          {skills.map((s) => (
            <li key={s.path}>
              <strong>{s.name}</strong>
              <code className="skill-path">{s.path}</code>
              {s.description && <p>{s.description}</p>}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
