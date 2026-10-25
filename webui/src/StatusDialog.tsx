import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import { getSettings, getStatus, saveSettings, type Settings, type Status } from "./api";
import { diffSettings, duration, pendingText, withCurrent } from "./settings";

type Note = { kind: "" | "ok" | "error"; text: string };

function StatusList({ status, error }: { status: Status | null; error: string }) {
  const onOff = (b: boolean | undefined) => (b ? "on" : "off");
  let rows: [string, string][];
  if (error) rows = [["Error", "Cannot load the status (" + error + ")."]];
  else if (!status) rows = [["Status", "Loading…"]];
  else {
    const chans = (status.channels ?? []).map(
      (c) => c.name + " (" + (c.running ? "running" : c.enabled ? "enabled, not running" : "disabled") + ")",
    );
    rows = [
      ["Version", status.version + (status.go ? " (" + status.go + ")" : "")],
      ["Uptime", duration(status.uptime_seconds || 0)],
      ["Model", status.model || "—"],
      ["Provider", status.provider || "—"],
      ["Workspace", status.workspace || "—"],
      ["Channels", chans.length ? chans.join(", ") : "none"],
      ["API", onOff(status.api_enabled)],
      ["Tools", "exec " + onOff(status.tools?.exec) + ", web_fetch " + onOff(status.tools?.web_fetch)],
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

  const load = useCallback(async () => {
    setStatus(null);
    setStatusError("");
    setNote({ kind: "", text: "Loading…" });
    getStatus().then(setStatus, (err: Error) => setStatusError(err.message));
    try {
      const view = await getSettings();
      setSaved(view.settings);
      setDraft(view.settings);
      setChoices(view.choices);
      setNote({ kind: "", text: pendingText(view) });
    } catch (err) {
      setSaved(null);
      setDraft(null);
      setNote({ kind: "error", text: "Cannot load the settings (" + (err as Error).message + ")." });
    }
  }, []);

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
      setNote({ kind: "", text: "Nothing changed." });
      return;
    }
    if (
      patch.channels?.websocket?.enabled === false &&
      !confirm("Disable the websocket channel?\n\nThis page will be gone after the gateway restarts.")
    ) {
      return;
    }
    setSaving(true);
    setNote({ kind: "", text: "Saving…" });
    try {
      const view = await saveSettings(patch);
      setSaved(view.settings);
      setDraft(view.settings);
      setChoices(view.choices);
      setNote({ kind: "ok", text: "Saved — restart the gateway to apply. " + pendingText(view) });
    } catch (err) {
      setNote({ kind: "error", text: "Not saved: " + (err as Error).message });
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
      <h2 id="status-title">Gateway status</h2>
      <StatusList status={status} error={statusError} />
      <p className="status-note">What the gateway is running with now.</p>
      <form id="settings-form" autoComplete="off" onSubmit={submit}>
        <h3>Settings</h3>
        <fieldset id="settings-fields" disabled={disabled}>
          <p className="status-note">
            Model, provider and fallback models are edited on the{" "}
            <a href="#/models" onClick={() => dialog.current?.close()}>
              Models page
            </a>
            .
          </p>
          <label className="check">
            <input type="checkbox" checked={draft?.tools.exec ?? false} onChange={(ev) => tool("exec", ev.target.checked)} />{" "}
            exec tool
          </label>
          <label className="check">
            <input
              type="checkbox"
              checked={draft?.tools.web_fetch ?? false}
              onChange={(ev) => tool("web_fetch", ev.target.checked)}
            />{" "}
            web_fetch tool
          </label>
          <label>
            Exec sandbox
            <select
              id="set-exec-sandbox"
              value={draft?.tools.exec_sandbox ?? ""}
              onChange={(ev) => tool("exec_sandbox", ev.target.value)}
            >
              {draft &&
                withCurrent(choices.exec_sandbox, draft.tools.exec_sandbox).map((v) => (
                  <option key={v} value={v}>
                    {v || "none"}
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
                    {name} channel
                  </label>
                ))}
          </div>
        </fieldset>
        <p className="status-note">
          Saved to config.json; the gateway uses saved settings after a restart. Keys, tokens and allow lists are edited
          in config.json only.
        </p>
        <p id="settings-message" className="status-note" role="status" data-kind={note.kind || undefined}>
          {note.text}
        </p>
        <div className="status-actions">
          <button type="submit" disabled={disabled}>
            Save
          </button>
          <button type="button" onClick={load} disabled={saving}>
            Refresh
          </button>
          <button type="button" onClick={() => dialog.current?.close()}>
            Close
          </button>
        </div>
      </form>
    </dialog>
  );
}
