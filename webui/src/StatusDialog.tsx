import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { getSettings, getStatus, saveSettings, type Settings, type Status } from "./api";
import { RestartGateway } from "./RestartGateway";
import { diffSettings, duration, pendingText, withCurrent } from "./settings";
import { useT } from "./i18n";

type Note = { kind: "" | "ok" | "error"; text: string };

function StatusList({ status, error }: { status: Status | null; error: string }) {
  const t = useT();
  const onOff = (b: boolean | undefined) => t(b ? "status.on" : "status.off");
  let rows: [string, string][];
  if (error) rows = [[t("status.rows.error"), t("status.loadError", { error })]];
  else if (!status) rows = [[t("status.rows.status"), t("common.loading")]];
  else {
    const chans = (status.channels ?? []).map((c) =>
      t("status.channelState", {
        name: c.name,
        state: t(c.running ? "common.running" : c.enabled ? "common.enabledNotRunning" : "common.disabled"),
      }),
    );
    rows = [
      [t("status.rows.version"), status.version + (status.go ? " (" + status.go + ")" : "")],
      [t("status.rows.uptime"), duration(status.uptime_seconds || 0)],
      [t("status.rows.model"), status.model || "—"],
      [t("status.rows.provider"), status.provider || "—"],
      [t("status.rows.workspace"), status.workspace || "—"],
      [t("status.rows.channels"), chans.length ? chans.join(", ") : t("common.none")],
      [t("status.rows.api"), onOff(status.api_enabled)],
      [t("status.rows.tools"), t("status.tools", { exec: onOff(status.tools?.exec), web_fetch: onOff(status.tools?.web_fetch) })],
    ];
  }
  return (
    <dl id="status-list">
      {rows.map(([name, value]) => (
        <div key={name}>
          <dt>{name}</dt>
          <dd>{value}</dd>
        </div>
      ))}
    </dl>
  );
}

// StatusDialog shows GET /ui/status and edits the tool and channel
// settings with GET and PATCH /ui/settings; the Models page owns the
// model settings. Esc or Close closes it.
export function StatusDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const t = useT();
  const dialog = useRef<HTMLDialogElement>(null);
  const [status, setStatus] = useState<Status | null>(null);
  const [statusError, setStatusError] = useState("");
  const [saved, setSaved] = useState<Settings | null>(null); // last loaded or saved
  const [draft, setDraft] = useState<Settings | null>(null);
  const [choices, setChoices] = useState<{ provider: string[] | null; exec_sandbox: string[] | null }>({
    provider: null,
    exec_sandbox: null,
  });
  const [saving, setSaving] = useState(false);
  const [note, setNote] = useState<Note>({ kind: "", text: "" });
  const [restartPending, setRestartPending] = useState(false);

  const load = useCallback(async () => {
    setStatus(null);
    setStatusError("");
    setNote({ kind: "", text: t("common.loading") });
    getStatus().then(setStatus, (err: Error) => setStatusError(err.message));
    try {
      const view = await getSettings();
      setSaved(view.settings);
      setDraft(view.settings);
      setChoices(view.choices);
      setNote({ kind: "", text: pendingText(view, t) });
      setRestartPending((view.restart_required ?? []).length > 0);
    } catch (err) {
      setSaved(null);
      setDraft(null);
      setNote({ kind: "error", text: t("status.settingsLoadError", { error: (err as Error).message }) });
    }
  }, [t]);

  useEffect(() => {
    const el = dialog.current;
    if (!el) return;
    if (open && !el.open) {
      el.showModal();
      load();
    } else if (!open && el.open) {
      el.close();
    }
  }, [open, load]);

  const submit = async (ev: FormEvent) => {
    ev.preventDefault();
    if (saving || !saved || !draft) return;
    const patch = diffSettings(saved, draft);
    if (!Object.keys(patch).length) {
      setNote({ kind: "", text: t("common.nothingChanged") });
      return;
    }
    if (
      patch.channels?.websocket?.enabled === false &&
      !confirm(t("status.disableWebsocket"))
    ) {
      return;
    }
    setSaving(true);
    setNote({ kind: "", text: t("common.saving") });
    try {
      const view = await saveSettings(patch);
      setSaved(view.settings);
      setDraft(view.settings);
      setChoices(view.choices);
      setNote({ kind: "ok", text: t("status.saved", { pending: pendingText(view, t) }).trim() });
      setRestartPending((view.restart_required ?? []).length > 0);
    } catch (err) {
      setNote({ kind: "error", text: t("common.notSaved", { message: (err as Error).message }) });
    } finally {
      setSaving(false);
    }
  };

  const edit = (change: (d: Settings) => Settings) => setDraft((d) => (d ? change(d) : d));
  const tool = (key: "exec" | "web_fetch" | "exec_sandbox", value: boolean | string) =>
    edit((d) => ({ ...d, tools: { ...d.tools, [key]: value } }));
  const disabled = !draft || saving;

  return (
    <dialog id="status-panel" ref={dialog} aria-labelledby="status-title" onClose={onClose}>
      <h2 id="status-title">{t("status.title")}</h2>
      <StatusList status={status} error={statusError} />
      <p className="status-note">{t("status.note")}</p>
      <form id="settings-form" autoComplete="off" onSubmit={submit}>
        <h3>{t("status.settings")}</h3>
        <fieldset id="settings-fields" disabled={disabled}>
          <p className="status-note">
            {t("status.modelsBefore")}{" "}
            <a href="#/models" onClick={() => dialog.current?.close()}>
              {t("status.modelsLink")}
            </a>
            {t("status.modelsAfter")}
          </p>
          <label className="check">
            <input type="checkbox" checked={draft?.tools.exec ?? false} onChange={(ev) => tool("exec", ev.target.checked)} />{" "}
            {t("status.execTool")}
          </label>
          <label className="check">
            <input
              type="checkbox"
              checked={draft?.tools.web_fetch ?? false}
              onChange={(ev) => tool("web_fetch", ev.target.checked)}
            />{" "}
            {t("status.webFetchTool")}
          </label>
          <label>
            {t("status.execSandbox")}
            <select
              id="set-exec-sandbox"
              value={draft?.tools.exec_sandbox ?? ""}
              onChange={(ev) => tool("exec_sandbox", ev.target.value)}
            >
              {draft &&
                withCurrent(choices.exec_sandbox, draft.tools.exec_sandbox).map((v) => (
                  <option key={v} value={v}>
                    {v || t("common.none")}
                  </option>
                ))}
            </select>
          </label>
          <div id="set-channels">
            {draft &&
              Object.keys(draft.channels)
                .sort()
                .map((name) => (
                  <label key={name} className="check">
                    <input
                      type="checkbox"
                      checked={draft.channels[name].enabled}
                      onChange={(ev) =>
                        edit((d) => ({ ...d, channels: { ...d.channels, [name]: { enabled: ev.target.checked } } }))
                      }
                    />{" "}
                    {t("status.channel", { name })}
                  </label>
                ))}
          </div>
        </fieldset>
        <p className="status-note">
          {t("status.savedNote")}
        </p>
        <p id="settings-message" className="status-note" role="status" data-kind={note.kind || undefined}>
          {note.text}
        </p>
        {restartPending && <RestartGateway />}
        <div className="status-actions">
          <button type="submit" disabled={disabled}>
            {t("common.save")}
          </button>
          <button type="button" onClick={load} disabled={saving}>
            {t("common.refresh")}
          </button>
          <button type="button" onClick={() => dialog.current?.close()}>
            {t("common.close")}
          </button>
        </div>
      </form>
    </dialog>
  );
}
