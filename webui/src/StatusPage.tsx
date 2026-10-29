import { useCallback, useEffect, useState, type FormEvent, type ReactNode } from "react";
import {
  getSettings,
  getStatus,
  saveSettings,
  type ImageGenerationSettings,
  type Settings,
  type SettingsChoices,
  type Status,
  type WebSearchSettings,
} from "./api";
import { Icon } from "./Icon";
import { RestartButton, RestartGateway } from "./RestartGateway";
import { diffSettings, duration, pendingText, savedText, whitelistLines, withCurrent } from "./settings";
import { TagInput } from "./TagInput";
import { useT } from "./i18n";

type Note = { kind: "" | "ok" | "error"; text: string };

// Switch is a toggle switch over a checkbox.
function Switch({ id, checked, onChange, label }: { id?: string; checked: boolean; onChange: (v: boolean) => void; label: string }) {
  return (
    <label className="switch" title={label}>
      <input id={id} type="checkbox" checked={checked} aria-label={label} onChange={(ev) => onChange(ev.target.checked)} />
      <span />
    </label>
  );
}

// Row is a settings row: a label (with help under it) and its control.
function Row({ label, help, htmlFor, top, children }: { label: ReactNode; help?: ReactNode; htmlFor?: string; top?: boolean; children: ReactNode }) {
  return (
    <div className={"form-row" + (top ? " top" : "")}>
      <label htmlFor={htmlFor}>
        {label}
        {help && <span className="help">{help}</span>}
      </label>
      <div className="ctl">{children}</div>
    </div>
  );
}

// Stats shows GET /ui/status: the stat row, then the runtime facts.
function Stats({ status, error }: { status: Status | null; error: string }) {
  const t = useT();
  if (error) return <p className="page-error">{t("status.loadError", { error })}</p>;
  if (!status) return <p className="page-empty">{t("common.loading")}</p>;
  const chans = status.channels ?? [];
  const running = chans.filter((c) => c.running).length;
  const [up, unit] = duration(status.uptime_seconds || 0).split(" ");
  return (
    <>
      <div className="stat-row" id="status-list">
        <div className="stat">
          <small>{t("status.rows.version")}</small>
          <strong className="mono">{status.version}</strong>
        </div>
        <div className="stat">
          <small>{t("status.rows.uptime")}</small>
          <strong>
            {up}
            {unit && <span className="unit"> {unit}</span>}
          </strong>
        </div>
        <div className="stat">
          <small>{t("status.rows.model")}</small>
          <strong className="mono sm">{status.model || "—"}</strong>
        </div>
        <div className="stat">
          <small>{t("status.rows.channels")}</small>
          <strong>
            {running}
            <span className="unit">{t("status.channelsUp", { total: chans.length })}</span>
          </strong>
        </div>
      </div>
      <div className="form-card">
        <div className="form-row">
          <span className="lbl">{t("status.rows.workspace")}</span>
          <div className="ctl">
            <code className="mono-val">{status.workspace || "—"}</code>
          </div>
        </div>
        <div className="form-row">
          <span className="lbl">{t("status.rows.provider")}</span>
          <div className="ctl">
            <span className="badge">{status.provider || "—"}</span>
          </div>
        </div>
        <div className="form-row">
          <span className="lbl">{t("status.rows.api")}</span>
          <div className="ctl">
            <span className={"badge" + (status.api_enabled ? " ok" : "")}>{t(status.api_enabled ? "status.on" : "status.off")}</span>
          </div>
        </div>
        <div className="form-row">
          <span className="lbl">{t("status.rows.channels")}</span>
          <div className="ctl wrap">
            {chans.length === 0
              ? t("common.none")
              : chans.map((c) => (
                  <span key={c.name} className={"badge" + (c.running ? " ok" : c.enabled ? " warn" : "")}>
                    {c.running && <span className="dot ok" />}
                    {t("status.channelState", {
                      name: c.name,
                      state: t(c.running ? "common.running" : c.enabled ? "common.enabledNotRunning" : "common.disabled"),
                    })}
                  </span>
                ))}
          </div>
        </div>
        {status.go && (
          <div className="form-row">
            <span className="lbl">{t("status.rows.runtime")}</span>
            <div className="ctl">
              <span className="badge mono">{status.go}</span>
            </div>
          </div>
        )}
      </div>
    </>
  );
}

// StatusPage is the settings shell's Status page: GET /ui/status as
// stats, then the tool and channel settings it edits with GET and PATCH
// /ui/settings; the Models page owns the model settings.
export function StatusPage() {
  const t = useT();
  const [status, setStatus] = useState<Status | null>(null);
  const [statusError, setStatusError] = useState("");
  const [saved, setSaved] = useState<Settings | null>(null); // last loaded or saved
  const [draft, setDraft] = useState<Settings | null>(null);
  const [choices, setChoices] = useState<SettingsChoices>({
    provider: null,
    exec_sandbox: null,
    image_generation_provider: null,
    web_provider: null,
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
    void load();
  }, [load]);

  const submit = async (ev: FormEvent) => {
    ev.preventDefault();
    if (saving || !saved || !draft) return;
    const patch = diffSettings(saved, draft);
    if (!Object.keys(patch).length) {
      setNote({ kind: "", text: t("common.nothingChanged") });
      return;
    }
    if (patch.channels?.websocket?.enabled === false && !confirm(t("status.disableWebsocket"))) {
      return;
    }
    setSaving(true);
    setNote({ kind: "", text: t("common.saving") });
    try {
      const view = await saveSettings(patch);
      setSaved(view.settings);
      setDraft(view.settings);
      setChoices(view.choices);
      setNote({ kind: "ok", text: savedText(view, t) });
      setRestartPending((view.restart_required ?? []).length > 0);
      getStatus().then(setStatus, () => {});
    } catch (err) {
      setNote({ kind: "error", text: t("common.notSaved", { message: (err as Error).message }) });
    } finally {
      setSaving(false);
    }
  };

  const edit = (change: (d: Settings) => Settings) => setDraft((d) => (d ? change(d) : d));
  const tool = (key: "exec" | "web_fetch" | "exec_sandbox", value: boolean | string) =>
    edit((d) => ({ ...d, tools: { ...d.tools, [key]: value } }));
  const image = <K extends keyof ImageGenerationSettings>(key: K, value: ImageGenerationSettings[K]) =>
    edit((d) => ({ ...d, tools: { ...d.tools, image_generation: { ...d.tools.image_generation, [key]: value } } }));
  const search = <K extends keyof WebSearchSettings>(key: K, value: WebSearchSettings[K]) =>
    edit((d) => (d.tools.web_search ? { ...d, tools: { ...d.tools, web_search: { ...d.tools.web_search, [key]: value } } } : d));
  const disabled = !draft || saving;
  const dirty = !!(saved && draft && Object.keys(diffSettings(saved, draft)).length);
  const ig = draft?.tools.image_generation;
  const ws = draft?.tools.web_search;

  return (
    <section className="page" aria-labelledby="status-title">
      <header className="page-header">
        <div className="page-title">
          <h2 id="status-title">{t("status.title")}</h2>
          <p>{t("status.blurb")}</p>
        </div>
        <div className="actions">
          <button type="button" className="btn ghost" onClick={() => void load()} disabled={saving}>
            <Icon name="refresh" />
            {t("common.refresh")}
          </button>
          <RestartButton className="btn danger" />
        </div>
      </header>

      <Stats status={status} error={statusError} />

      <form id="settings-form" autoComplete="off" onSubmit={submit}>
        <div className="section-title">
          {t("status.toolSettings")}
          <span className="line" />
        </div>
        <fieldset id="settings-fields" className="form-card" disabled={disabled}>
          <Row label={t("status.execTool")} help={t("status.execHelp")} htmlFor="set-exec">
            <Switch id="set-exec" label={t("status.execTool")} checked={draft?.tools.exec ?? false} onChange={(v) => tool("exec", v)} />
          </Row>
          <Row label={t("status.execSandbox")} htmlFor="set-exec-sandbox">
            <select
              id="set-exec-sandbox"
              className="select"
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
          </Row>
          <Row label={t("status.webFetchTool")} help={t("status.webFetchHelp")} htmlFor="set-web-fetch">
            <Switch
              id="set-web-fetch"
              label={t("status.webFetchTool")}
              checked={draft?.tools.web_fetch ?? false}
              onChange={(v) => tool("web_fetch", v)}
            />
          </Row>
          <Row label={t("status.ssrfWhitelist")} help={t("status.ssrfWhitelistHint")} htmlFor="set-ssrf-whitelist" top>
            <TagInput
              id="set-ssrf-whitelist"
              value={whitelistLines(draft?.tools.ssrf_whitelist ?? null)}
              onChange={(next) => edit((d) => ({ ...d, tools: { ...d.tools, ssrf_whitelist: next } }))}
              placeholder={t("status.ssrfPlaceholder")}
              removeLabel={(entry) => t("status.ssrfRemove", { entry })}
              disabled={disabled}
            />
          </Row>
          {ws && (
            <>
              <Row label={t("status.webSearchProvider")} htmlFor="set-web-search-provider">
                <select
                  id="set-web-search-provider"
                  className="select"
                  value={ws.provider}
                  onChange={(ev) => search("provider", ev.target.value)}
                >
                  {withCurrent(choices.web_provider ?? null, ws.provider).map((v) => (
                    <option key={v} value={v}>
                      {v || t("common.none")}
                    </option>
                  ))}
                </select>
              </Row>
              {ws.provider && (
                <Row label={t("status.webSearchLimits")} htmlFor="set-web-search-max">
                  <input
                    id="set-web-search-max"
                    className="input num"
                    type="number"
                    required
                    min={1}
                    max={50}
                    step={1}
                    title={t("status.webSearchMax")}
                    aria-label={t("status.webSearchMax")}
                    value={ws.max_results}
                    onChange={(ev) => search("max_results", ev.target.valueAsNumber)}
                  />
                  <input
                    id="set-web-search-timeout"
                    className="input num"
                    type="number"
                    required
                    min={1}
                    max={300}
                    step={1}
                    title={t("status.webSearchTimeout")}
                    aria-label={t("status.webSearchTimeout")}
                    value={ws.timeout}
                    onChange={(ev) => search("timeout", ev.target.valueAsNumber)}
                  />
                </Row>
              )}
              <Row
                label={t("status.webFetchProvider")}
                htmlFor="set-web-fetch-provider"
                help={
                  <>
                    {ws.key_source ? t("status.webSearchKeySet", { source: ws.key_source }) : t("status.webSearchKeyMissing")}{" "}
                    <a href="#/providers">{t("status.imageKeyLink")}</a>
                    {t("status.webSearchKeyAfter")}
                  </>
                }
                top
              >
                <select
                  id="set-web-fetch-provider"
                  className="select"
                  value={draft?.tools.web_fetch_provider ?? ""}
                  onChange={(ev) => edit((d) => ({ ...d, tools: { ...d.tools, web_fetch_provider: ev.target.value } }))}
                >
                  {withCurrent(choices.web_provider ?? null, draft?.tools.web_fetch_provider ?? "").map((v) => (
                    <option key={v} value={v}>
                      {v || t("status.webFetchDirect")}
                    </option>
                  ))}
                </select>
              </Row>
            </>
          )}
          <Row
            label={t("status.imageGeneration")}
            htmlFor="set-image-enabled"
            help={
              <>
                {t("status.imageKeyBefore")} <a href="#/providers">{t("status.imageKeyLink")}</a>
                {t("status.imageKeyAfter")}
              </>
            }
          >
            <Switch id="set-image-enabled" label={t("status.imageGeneration")} checked={ig?.enabled ?? false} onChange={(v) => image("enabled", v)} />
          </Row>
          {ig?.enabled && (
            <div id="set-image-generation" className="form-sub">
              <Row label={t("status.imageProvider")} htmlFor="set-image-provider">
                <select id="set-image-provider" className="select" value={ig.provider} onChange={(ev) => image("provider", ev.target.value)}>
                  {withCurrent(choices.image_generation_provider, ig.provider).map((v) => (
                    <option key={v} value={v}>
                      {v || t("common.none")}
                    </option>
                  ))}
                </select>
              </Row>
              <Row label={t("status.imageModel")} htmlFor="set-image-model">
                <input id="set-image-model" className="input mono wide" type="text" required value={ig.model} onChange={(ev) => image("model", ev.target.value)} />
              </Row>
              <Row label={t("status.imageAspectRatio")} htmlFor="set-image-aspect">
                <input
                  id="set-image-aspect"
                  className="input num"
                  type="text"
                  required
                  placeholder="1:1"
                  value={ig.default_aspect_ratio}
                  onChange={(ev) => image("default_aspect_ratio", ev.target.value)}
                />
              </Row>
              <Row label={t("status.imageSize")} htmlFor="set-image-size">
                <input
                  id="set-image-size"
                  className="input num"
                  type="text"
                  required
                  placeholder="1K"
                  value={ig.default_image_size}
                  onChange={(ev) => image("default_image_size", ev.target.value)}
                />
              </Row>
              <Row label={t("status.imageMax")} htmlFor="set-image-max">
                <input
                  id="set-image-max"
                  className="input num"
                  type="number"
                  required
                  min={1}
                  max={8}
                  step={1}
                  value={ig.max_images_per_turn}
                  onChange={(ev) => image("max_images_per_turn", ev.target.valueAsNumber)}
                />
              </Row>
            </div>
          )}
        </fieldset>

        {draft && Object.keys(draft.channels).length > 0 && (
          <>
            <div className="section-title">
              {t("status.channelSettings")}
              <span className="line" />
            </div>
            <fieldset id="set-channels" className="form-card" disabled={disabled}>
              {Object.keys(draft.channels)
                .sort()
                .map((name) => (
                  <Row
                    key={name}
                    label={t("status.channel", { name })}
                    help={name === "websocket" ? t("status.websocketHelp") : undefined}
                    htmlFor={"set-channel-" + name}
                  >
                    <Switch
                      id={"set-channel-" + name}
                      label={t("status.channel", { name })}
                      checked={draft.channels[name].enabled}
                      onChange={(v) => edit((d) => ({ ...d, channels: { ...d.channels, [name]: { enabled: v } } }))}
                    />
                  </Row>
                ))}
            </fieldset>
          </>
        )}

        <div className="form-card form-actions">
          <div className="form-foot">
            <p id="settings-message" className="status-note" role="status" data-kind={note.kind || undefined}>
              {note.text}
            </p>
            <button type="button" className="btn ghost" disabled={disabled || !dirty} onClick={() => setDraft(saved)}>
              {t("status.discard")}
            </button>
            <button type="submit" className="btn primary" disabled={disabled}>
              {t("status.saveChanges")}
            </button>
          </div>
        </div>
        {restartPending && <RestartGateway />}
        <p className="status-note">
          {t("status.modelsBefore")} <a href="#/models">{t("status.modelsLink")}</a>
          {t("status.modelsAfter")} {t("status.savedNote")}
        </p>
      </form>
    </section>
  );
}
