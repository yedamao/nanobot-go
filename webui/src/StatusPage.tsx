import { useCallback, useEffect, useState, type FormEvent } from "react";
import { getSettings, getStatus, saveSettings, type Settings, type Status } from "./api";
import { Row, Switch } from "./FormControls";
import { Icon } from "./Icon";
import { RestartButton, RestartGateway } from "./RestartGateway";
import { duration, pendingText, savedText } from "./settings";
import { useT } from "./i18n";
import { SettingsCard } from "./SettingsCard";

type Note = { kind: "" | "ok" | "error"; text: string };

function Stats({ status, error }: { status: Status | null; error: string }) {
  const t = useT();
  if (error) return <p className="page-error">{t("status.loadError", { error })}</p>;
  if (!status) return <p className="page-empty">{t("common.loading")}</p>;
  const channels = status.channels ?? [];
  const running = channels.filter((channel) => channel.running).length;
  const [up, unit] = duration(status.uptime_seconds || 0).split(" ");
  return <>
    <div className="stat-row" id="status-list">
      <div className="stat"><small>{t("status.rows.version")}</small><strong className="mono">{status.version}</strong></div>
      <div className="stat"><small>{t("status.rows.uptime")}</small><strong>{up}{unit && <span className="unit"> {unit}</span>}</strong></div>
      <div className="stat"><small>{t("status.rows.model")}</small><strong className="mono sm">{status.model || "—"}</strong></div>
      <div className="stat"><small>{t("status.rows.channels")}</small><strong>{running}<span className="unit">{t("status.channelsUp", { total: channels.length })}</span></strong></div>
    </div>
    <SettingsCard icon="panel" title={t("status.runtimeTitle")} help={t("status.runtimeHelp")}>
      <div className="form-row"><span className="lbl">{t("status.rows.workspace")}</span><div className="ctl"><code className="mono-val">{status.workspace || "—"}</code></div></div>
      <div className="form-row"><span className="lbl">{t("status.rows.provider")}</span><div className="ctl"><span className="badge">{status.provider || "—"}</span></div></div>
      <div className="form-row"><span className="lbl">{t("status.rows.api")}</span><div className="ctl"><span className={"badge" + (status.api_enabled ? " ok" : "")}>{t(status.api_enabled ? "status.on" : "status.off")}</span></div></div>
      {status.go && <div className="form-row"><span className="lbl">{t("status.rows.runtime")}</span><div className="ctl"><span className="badge mono">{status.go}</span></div></div>}
      <div className="form-row"><span className="lbl">{t("status.rows.channels")}</span><div className="ctl wrap">{channels.length === 0 ? t("common.none") : channels.map((channel) => <span key={channel.name} className={"badge" + (channel.running ? " ok" : channel.enabled ? " warn" : "")}>{channel.running && <span className="dot ok" />}{t("status.channelState", { name: channel.name, state: t(channel.running ? "common.running" : channel.enabled ? "common.enabledNotRunning" : "common.disabled") })}</span>)}</div></div>
    </SettingsCard>
  </>;
}

export function StatusPage() {
  const t = useT();
  const [status, setStatus] = useState<Status | null>(null);
  const [statusError, setStatusError] = useState("");
  const [saved, setSaved] = useState<Settings | null>(null);
  const [draft, setDraft] = useState<Settings | null>(null);
  const [saving, setSaving] = useState(false);
  const [note, setNote] = useState<Note>({ kind: "", text: "" });
  const [restartPending, setRestartPending] = useState(false);

  const load = useCallback(async () => {
    setStatus(null); setStatusError(""); setNote({ kind: "", text: t("common.loading") });
    getStatus().then(setStatus, (error: Error) => setStatusError(error.message));
    try {
      const view = await getSettings();
      setSaved(view.settings); setDraft(view.settings);
      setNote({ kind: "", text: pendingText(view, t) });
      setRestartPending((view.restart_required ?? []).length > 0);
    } catch (error) {
      setSaved(null); setDraft(null);
      setNote({ kind: "error", text: t("status.settingsLoadError", { error: (error as Error).message }) });
    }
  }, [t]);
  useEffect(() => { void load(); }, [load]);

  const channelPatch = () => {
    const channels: Record<string, { enabled: boolean }> = {};
    if (!saved || !draft) return channels;
    for (const [name, channel] of Object.entries(draft.channels))
      if (saved.channels[name] && channel.enabled !== saved.channels[name].enabled) channels[name] = { enabled: channel.enabled };
    return channels;
  };
  const dirty = Object.keys(channelPatch()).length > 0;
  const disabled = !draft || saving;
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (disabled) return;
    const channels = channelPatch();
    if (!Object.keys(channels).length) { setNote({ kind: "", text: t("common.nothingChanged") }); return; }
    if (channels.websocket?.enabled === false && !confirm(t("status.disableWebsocket"))) return;
    setSaving(true); setNote({ kind: "", text: t("common.saving") });
    try {
      const view = await saveSettings({ channels });
      setSaved(view.settings); setDraft(view.settings);
      setNote({ kind: "ok", text: savedText(view, t) });
      setRestartPending((view.restart_required ?? []).length > 0);
      getStatus().then(setStatus, () => {});
    } catch (error) { setNote({ kind: "error", text: t("common.notSaved", { message: (error as Error).message }) }); }
    finally { setSaving(false); }
  };

  return <section className="page" aria-labelledby="status-title">
    <header className="page-header"><div className="page-title"><h2 id="status-title">{t("status.title")}</h2><p>{t("status.blurb")}</p></div><div className="actions">
      <button type="button" className="btn ghost" onClick={() => void load()} disabled={saving}><Icon name="refresh" />{t("common.refresh")}</button><RestartButton className="btn danger" />
    </div></header>
    <div className="section-stack"><Stats status={status} error={statusError} /></div>
    <form id="settings-form" autoComplete="off" onSubmit={submit}>
      {draft && Object.keys(draft.channels).length > 0 && <fieldset id="set-channels" className="settings-fieldset" disabled={disabled}><SettingsCard icon="radio" title={t("status.channelSettings")} help={t("status.channelsHelp")}>{Object.keys(draft.channels).sort().map((name) =>
          <Row key={name} label={t("status.channel", { name })} help={name === "websocket" ? t("status.websocketHelp") : undefined} htmlFor={"set-channel-" + name}>
            <Switch id={"set-channel-" + name} label={t("status.channel", { name })} checked={draft.channels[name].enabled} onChange={(enabled) => setDraft((current) => current ? ({ ...current, channels: { ...current.channels, [name]: { enabled } } }) : current)} />
          </Row>)}</SettingsCard></fieldset>}
      <div className="save-bar"><p id="settings-message" className="status-note" role="status" data-kind={note.kind || undefined}>{note.text}</p><div className="actions">
        <button type="button" className="btn ghost" disabled={disabled || !dirty} onClick={() => setDraft(saved)}>{t("status.discard")}</button>
        <button type="submit" className="btn primary" disabled={disabled || !dirty}>{t("status.saveChanges")}</button>
      </div></div>
      {restartPending && <RestartGateway />}
    </form>
  </section>;
}
