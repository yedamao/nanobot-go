import { useCallback, useEffect, useState } from "react";
import { deleteSkill, installSkill, listSkills, setSkillEnabled, type Skill, type SkillsList } from "./api";
import { groupSkills, skillBadges } from "./skills";
import { Fill, useT, type MessageKey } from "./i18n";

export function SkillsPage() {
  const t = useT();
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

  // act runs one change for skill s, then reloads the list; done and
  // failed are its notes.
  const act = async (s: Skill, done: MessageKey, failed: MessageKey, change: () => Promise<unknown>) => {
    setBusy(s.name);
    setNote("");
    try {
      await change();
      setNote(t(done, { name: s.name }));
      await load();
    } catch (err) {
      setNote(t(failed, { message: (err as Error).message }));
    } finally {
      setBusy(null);
    }
  };

  const toggle = (s: Skill) =>
    s.enabled
      ? act(s, "skills.disabledNote", "skills.disableFailed", () => setSkillEnabled(s.name, false))
      : act(s, "skills.enabledNote", "skills.enableFailed", () => setSkillEnabled(s.name, true));
  const install = (s: Skill) => act(s, "skills.installed", "skills.installFailed", () => installSkill(s.name));
  const remove = (s: Skill) => {
    if (!confirm(t("skills.deleteConfirm", { name: s.name }))) return;
    void act(s, "skills.deleted", "skills.deleteFailed", () => deleteSkill(s.name));
  };

  const groups = list ? groupSkills(list.skills) : null;
  const canEdit = !!list?.persistent;

  return (
    <section className="page" aria-labelledby="skills-heading">
      <header className="page-header">
        <h2 id="skills-heading">{t("skills.heading")}</h2>
        <button type="button" onClick={() => void load()} disabled={busy !== null}>
          {t("common.refresh")}
        </button>
      </header>
      {error && <p className="page-error">{t("skills.loadError", { error })}</p>}
      {note && <p className="page-note">{note}</p>}
      {!error && list === null && <p className="page-empty">{t("common.loading")}</p>}
      {groups && (
        <>
          <h3>{t("skills.workspace")}</h3>
          {groups.installed.length === 0 ? (
            <p className="page-empty">
              <Fill text={t("skills.noWorkspace")} vars={{ path: <code>skills/&lt;name&gt;/SKILL.md</code> }} />
            </p>
          ) : (
            <ul className="page-list skills-list">
              {groups.installed.map((s) => (
                <li key={s.name} data-disabled={s.enabled ? undefined : ""}>
                  <div className="skill-head">
                    <strong>{s.name}</strong>
                    {skillBadges(s, t).map((b) => (
                      <span key={b} className="badge">
                        {b}
                      </span>
                    ))}
                    {canEdit && (
                      <span className="page-actions">
                        <button type="button" disabled={busy !== null} onClick={() => void toggle(s)}>
                          {t(s.enabled ? "common.disable" : "common.enable")}
                        </button>
                        <button type="button" disabled={busy !== null} onClick={() => remove(s)}>
                          {t("skills.delete")}
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
              <h3>{t("skills.bundled")}</h3>
              <ul className="page-list skills-list">
                {groups.available.map((s) => (
                  <li key={s.name}>
                    <div className="skill-head">
                      <strong>{s.name}</strong>
                      {canEdit && (
                        <span className="page-actions">
                          <button type="button" disabled={busy !== null} onClick={() => void install(s)}>
                            {t("skills.install")}
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
            {t("skills.help")}
          </p>
        </>
      )}
    </section>
  );
}
