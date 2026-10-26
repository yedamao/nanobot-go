import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { getProviderModels, getSettings, saveSettings, type FallbackModel, type Settings, type SettingsView } from "./api";
import {
  catalogListId,
  catalogNote,
  catalogOptions,
  checkModels,
  diffModels,
  draftProviders,
  maxFallbacks,
  modelsDraft,
  moveItem,
  type Catalog,
  type ModelsDraft,
} from "./models";
import { pendingText, withCurrent } from "./settings";
import { RestartGateway } from "./RestartGateway";
import { Fill, useT } from "./i18n";

// ModelsPage edits agents.defaults.model, .provider and the ordered
// .fallbackModels through GET and PATCH /ui/settings, the same store the
// Status dialog reads. Provider keys are set on the Providers page. Each
// model field suggests IDs from its row's provider catalog (GET
// /ui/providers/{name}/models), advisory only: any ID can be typed.
export function ModelsPage() {
  const t = useT();
  const [view, setView] = useState<SettingsView | null>(null);
  const [draft, setDraft] = useState<ModelsDraft | null>(null);
  const [error, setError] = useState("");
  const [note, setNote] = useState<{ kind: "" | "ok" | "error"; text: string }>({ kind: "", text: "" });
  const [saving, setSaving] = useState(false);
  const [catalogs, setCatalogs] = useState<Record<string, Catalog>>({});
  // requested holds the providers whose catalog was asked for, so each
  // is fetched once until Refresh.
  const requested = useRef(new Set<string>());

  const show = (v: SettingsView) => {
    setView(v);
    setDraft(modelsDraft(v.settings));
  };

  const load = useCallback(async () => {
    requested.current.clear();
    setCatalogs({});
    setError("");
    setNote({ kind: "", text: "" });
    try {
      show(await getSettings());
    } catch (err) {
      setView(null);
      setDraft(null);
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // Fetch the catalog of every provider a row uses, once each.
  const providerKey = draft ? draftProviders(draft).join("\n") : "";
  useEffect(() => {
    for (const name of providerKey ? providerKey.split("\n") : []) {
      if (requested.current.has(name)) continue;
      requested.current.add(name);
      setCatalogs((c) => ({ ...c, [name]: { state: "loading" } }));
      getProviderModels(name).then(
        (models) => setCatalogs((c) => ({ ...c, [name]: { state: "ok", models } })),
        (err: Error) => setCatalogs((c) => ({ ...c, [name]: { state: "error", error: err.message } })),
      );
    }
  }, [providerKey]);

  const submit = async (ev: FormEvent) => {
    ev.preventDefault();
    if (saving || !view || !draft) return;
    const bad = checkModels(draft);
    if (bad) {
      setNote({ kind: "error", text: t("common.notSavedInvalid", { message: t(bad.key, bad.vars) }) });
      return;
    }
    const patch = diffModels(view.settings, draft);
    if (!Object.keys(patch).length) {
      setNote({ kind: "", text: t("common.nothingChanged") });
      return;
    }
    setSaving(true);
    setNote({ kind: "", text: t("common.saving") });
    try {
      show(await saveSettings(patch));
      setNote({ kind: "ok", text: t("common.savedToConfig") });
    } catch (err) {
      setNote({ kind: "error", text: t("common.notSaved", { message: (err as Error).message }) });
    } finally {
      setSaving(false);
    }
  };

  const edit = (change: (d: ModelsDraft) => ModelsDraft) => setDraft((d) => (d ? change(d) : d));
  const editFallback = (i: number, patch: Partial<FallbackModel>) =>
    edit((d) => ({ ...d, fallbacks: d.fallbacks.map((f, j) => (j === i ? { ...f, ...patch } : f)) }));
  const providers = (current: string) =>
    withCurrent(view?.choices.provider ?? null, current).map((v) => (
      <option key={v} value={v}>
        {v}
      </option>
    ));
  const catalogHint = (provider: string) => {
    const text = catalogNote(catalogs[provider || "auto"], t);
    return text ? <small className="models-catalog-note">{text}</small> : null;
  };
  const saved: Settings | undefined = view?.settings;
  const dirty = !!(saved && draft && Object.keys(diffModels(saved, draft)).length);
  const pending = view ? pendingText(view, t) : "";

  return (
    <section className="page" aria-labelledby="models-heading">
      <header className="page-header">
        <h2 id="models-heading">{t("models.heading")}</h2>
        <button type="button" onClick={() => void load()} disabled={saving}>
          {t("common.refresh")}
        </button>
      </header>
      {error && <p className="page-error">{t("models.loadError", { error })}</p>}
      {!error && !draft && <p className="page-empty">{t("common.loading")}</p>}
      {pending && <p className="page-note">{t("models.pendingNote", { pending })}</p>}
      {(view?.restart_required ?? []).length > 0 && <RestartGateway />}
      {draft && (
        <form className="cron-form" autoComplete="off" onSubmit={(e) => void submit(e)}>
          <h3>{t("models.primary")}</h3>
          <fieldset className="cron-fields" disabled={saving}>
            <label htmlFor="models-model">
              {t("models.model")}
              <input
                id="models-model"
                type="text"
                maxLength={200}
                spellCheck={false}
                required
                list={catalogListId(draft.provider)}
                value={draft.model}
                onChange={(e) => edit((d) => ({ ...d, model: e.target.value }))}
              />
              {catalogHint(draft.provider)}
            </label>
            <label htmlFor="models-provider">
              {t("models.provider")}
              <select
                id="models-provider"
                value={draft.provider}
                onChange={(e) => edit((d) => ({ ...d, provider: e.target.value }))}
              >
                {providers(draft.provider)}
              </select>
            </label>
          </fieldset>

          <h3>{t("models.fallbacks")}</h3>
          <p className="page-empty">{t("models.fallbacksHelp")}</p>
          {draft.fallbacks.length === 0 ? (
            <p className="page-empty">{t("models.noFallbacks")}</p>
          ) : (
            <ol className="page-list models-fallbacks">
              {draft.fallbacks.map((f, i) => (
                <li key={i}>
                  <fieldset className="cron-fields" disabled={saving}>
                    <label>
                      {t("models.provider")}
                      <select
                        aria-label={t("models.fallbackProvider", { n: i + 1 })}
                        value={f.provider || "auto"}
                        onChange={(e) => editFallback(i, { provider: e.target.value })}
                      >
                        {providers(f.provider || "auto")}
                      </select>
                    </label>
                    <label>
                      {t("models.model")}
                      <input
                        aria-label={t("models.fallbackModel", { n: i + 1 })}
                        type="text"
                        maxLength={200}
                        spellCheck={false}
                        required
                        list={catalogListId(f.provider)}
                        value={f.model}
                        onChange={(e) => editFallback(i, { model: e.target.value })}
                      />
                      {catalogHint(f.provider)}
                    </label>
                  </fieldset>
                  <span className="page-actions">
                    <button
                      type="button"
                      disabled={saving || i === 0}
                      onClick={() => edit((d) => ({ ...d, fallbacks: moveItem(d.fallbacks, i, -1) }))}
                    >
                      {t("models.up")}
                    </button>
                    <button
                      type="button"
                      disabled={saving || i === draft.fallbacks.length - 1}
                      onClick={() => edit((d) => ({ ...d, fallbacks: moveItem(d.fallbacks, i, 1) }))}
                    >
                      {t("models.down")}
                    </button>
                    <button
                      type="button"
                      disabled={saving}
                      onClick={() => edit((d) => ({ ...d, fallbacks: d.fallbacks.filter((_, j) => j !== i) }))}
                    >
                      {t("common.remove")}
                    </button>
                  </span>
                </li>
              ))}
            </ol>
          )}
          <div className="cron-form-actions">
            <button
              type="button"
              disabled={saving || draft.fallbacks.length >= maxFallbacks}
              onClick={() => edit((d) => ({ ...d, fallbacks: [...d.fallbacks, { provider: "auto", model: "" }] }))}
            >
              {t("models.addFallback")}
            </button>
            <button type="button" disabled={saving || !dirty} onClick={() => saved && setDraft(modelsDraft(saved))}>
              {t("common.reset")}
            </button>
            <button type="submit" disabled={saving || !dirty}>
              {t("common.save")}
            </button>
          </div>
          {Object.entries(catalogs).map(([name, c]) => (
            <datalist key={name} id={catalogListId(name)}>
              {catalogOptions(c).map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </datalist>
          ))}
          {note.text && (
            <p className={note.kind === "error" ? "page-error" : "page-note"} role="status">
              {note.text}
            </p>
          )}
        </form>
      )}
      <p className="page-empty">
        <Fill
          text={t("models.help")}
          vars={{ path: <code>agents.defaults</code>, link: <a href="#/providers">{t("models.providersLink")}</a> }}
        />
      </p>
    </section>
  );
}
