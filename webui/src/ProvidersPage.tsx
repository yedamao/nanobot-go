import { useCallback, useEffect, useState, type FormEvent } from "react";
import { listProviders, saveProvider, type Provider } from "./api";
import { checkProvider, diffProvider, keyStatus, providerDraft, type ProviderDraft } from "./providers";

// ProvidersPage sets or clears each known provider's apiKey and edits its
// apiBase through GET and PATCH /ui/providers. Saved keys are never sent
// to the page: it shows whether one is set and a short hint.
export function ProvidersPage() {
  const [providers, setProviders] = useState<Provider[] | null>(null);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    setError("");
    try {
      setProviders(await listProviders());
    } catch (err) {
      setProviders(null);
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const saved = (p: Provider) => setProviders((list) => list && list.map((q) => (q.name === p.name ? p : q)));
  const pending = (providers ?? []).filter((p) => p.restart_required).map((p) => p.name);

  return (
    <section className="page" aria-labelledby="providers-heading">
      <header className="page-header">
        <h2 id="providers-heading">Providers</h2>
        <button type="button" onClick={() => void load()}>
          Refresh
        </button>
      </header>
      {error && <p className="page-error">Cannot load the providers ({error}).</p>}
      {!error && !providers && <p className="page-empty">Loading…</p>}
      {pending.length > 0 && (
        <p className="page-note">
          Restart the gateway to use the saved credentials of {pending.join(", ")}. Provider clients are built when the
          gateway starts.
        </p>
      )}
      {providers?.map((p) => <ProviderForm key={p.name} provider={p} onSaved={saved} />)}
      <p className="page-empty">
        Saved to <code>providers.&lt;name&gt;</code> in config.json. <code>NANOBOT_GO_&lt;NAME&gt;_API_KEY</code> and{" "}
        <code>_API_BASE</code> in the environment win over the file.
      </p>
    </section>
  );
}

function ProviderForm({ provider: p, onSaved }: { provider: Provider; onSaved: (p: Provider) => void }) {
  const [draft, setDraft] = useState<ProviderDraft>(() => providerDraft(p));
  const [note, setNote] = useState<{ kind: "" | "ok" | "error"; text: string }>({ kind: "", text: "" });
  const [saving, setSaving] = useState(false);

  useEffect(() => setDraft(providerDraft(p)), [p]);

  const submit = async (ev: FormEvent) => {
    ev.preventDefault();
    if (saving) return;
    const bad = checkProvider(draft);
    if (bad) {
      setNote({ kind: "error", text: "Not saved: " + bad + "." });
      return;
    }
    const patch = diffProvider(p, draft);
    if (!Object.keys(patch).length) {
      setNote({ kind: "", text: "Nothing changed." });
      return;
    }
    setSaving(true);
    setNote({ kind: "", text: "Saving…" });
    try {
      onSaved(await saveProvider(p.name, patch));
      setNote({ kind: "ok", text: "Saved to config.json." });
    } catch (err) {
      setNote({ kind: "error", text: "Not saved: " + (err as Error).message });
    } finally {
      setSaving(false);
    }
  };

  const id = "provider-" + p.name;
  const dirty = Object.keys(diffProvider(p, draft)).length > 0;
  return (
    <form className="cron-form" autoComplete="off" onSubmit={(e) => void submit(e)} aria-labelledby={id + "-heading"}>
      <h3 id={id + "-heading"}>
        {p.name}{" "}
        {p.restart_required && <span className="badge">restart required</span>}
      </h3>
      <p className="page-empty">API key: {keyStatus(p)}</p>
      <fieldset className="cron-fields" disabled={saving}>
        <label htmlFor={id + "-key"}>
          New API key
          <input
            id={id + "-key"}
            type="password"
            autoComplete="new-password"
            spellCheck={false}
            maxLength={4096}
            placeholder={p.hasApiKey ? "Leave empty to keep the saved key" : "Paste the key"}
            disabled={draft.clearKey}
            value={draft.apiKey}
            onChange={(e) => setDraft((d) => ({ ...d, apiKey: e.target.value }))}
          />
        </label>
        <label htmlFor={id + "-base"}>
          API base
          <input
            id={id + "-base"}
            type="text"
            inputMode="url"
            spellCheck={false}
            maxLength={2048}
            placeholder={p.defaultApiBase || "Default endpoint"}
            value={draft.apiBase}
            onChange={(e) => setDraft((d) => ({ ...d, apiBase: e.target.value }))}
          />
        </label>
      </fieldset>
      {p.env_overrides.includes("apiBase") && <p className="page-empty">The environment overrides the API base.</p>}
      <div className="cron-form-actions">
        <button
          type="button"
          disabled={saving || !p.hasApiKey}
          aria-pressed={draft.clearKey}
          onClick={() => setDraft((d) => ({ ...d, apiKey: "", clearKey: !d.clearKey }))}
        >
          {draft.clearKey ? "Keep key" : "Clear key"}
        </button>
        <button type="button" disabled={saving || !dirty} onClick={() => setDraft(providerDraft(p))}>
          Reset
        </button>
        <button type="submit" disabled={saving || !dirty}>
          Save
        </button>
      </div>
      {note.text && (
        <p className={note.kind === "error" ? "page-error" : "page-note"} role="status">
          {note.text}
        </p>
      )}
    </form>
  );
}
