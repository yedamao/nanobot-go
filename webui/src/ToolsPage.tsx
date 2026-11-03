import { useCallback, useEffect, useState, type FormEvent } from "react";
import { getSettings, saveSettings, type ImageGenerationSettings, type Settings, type SettingsChoices, type WebSearchSettings } from "./api";
import { Row, Switch } from "./FormControls";
import { Icon } from "./Icon";
import { RestartButton, RestartGateway } from "./RestartGateway";
import { diffSettings, pendingText, savedText, whitelistLines, withCurrent } from "./settings";
import { TagInput } from "./TagInput";
import { useT } from "./i18n";
import { SettingsCard } from "./SettingsCard";

type Note = { kind: "" | "ok" | "error"; text: string };

export function ToolsPage() {
  const t = useT();
  const [saved, setSaved] = useState<Settings | null>(null);
  const [draft, setDraft] = useState<Settings | null>(null);
  const [choices, setChoices] = useState<SettingsChoices>({ provider: null, exec_sandbox: null, image_generation_provider: null, web_provider: null });
  const [saving, setSaving] = useState(false);
  const [note, setNote] = useState<Note>({ kind: "", text: "" });
  const [restartPending, setRestartPending] = useState(false);

  const load = useCallback(async () => {
    setNote({ kind: "", text: t("common.loading") });
    try {
      const view = await getSettings();
      setSaved(view.settings); setDraft(view.settings); setChoices(view.choices);
      setNote({ kind: "", text: pendingText(view, t) });
      setRestartPending((view.restart_required ?? []).length > 0);
    } catch (error) {
      setSaved(null); setDraft(null);
      setNote({ kind: "error", text: t("tools.settingsLoadError", { error: (error as Error).message }) });
    }
  }, [t]);
  useEffect(() => { void load(); }, [load]);

  const edit = (change: (settings: Settings) => Settings) => setDraft((current) => current ? change(current) : current);
  const tool = (key: "exec" | "web_fetch" | "browser" | "exec_sandbox", value: boolean | string) => edit((settings) => ({ ...settings, tools: { ...settings.tools, [key]: value } }));
  const image = <K extends keyof ImageGenerationSettings>(key: K, value: ImageGenerationSettings[K]) => edit((settings) => ({ ...settings, tools: { ...settings.tools, image_generation: { ...settings.tools.image_generation, [key]: value } } }));
  const search = <K extends keyof WebSearchSettings>(key: K, value: WebSearchSettings[K]) => edit((settings) => settings.tools.web_search ? ({ ...settings, tools: { ...settings.tools, web_search: { ...settings.tools.web_search, [key]: value } } }) : settings);
  const toolsPatch = () => {
    if (!saved || !draft) return undefined;
    return diffSettings(saved, { ...draft, channels: saved.channels }).tools;
  };
  const dirty = !!toolsPatch();
  const disabled = !draft || saving;
  const ig = draft?.tools.image_generation;
  const ws = draft?.tools.web_search;

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (disabled) return;
    const tools = toolsPatch();
    if (!tools) { setNote({ kind: "", text: t("common.nothingChanged") }); return; }
    setSaving(true); setNote({ kind: "", text: t("common.saving") });
    try {
      const view = await saveSettings({ tools });
      setSaved(view.settings); setDraft(view.settings); setChoices(view.choices);
      setNote({ kind: "ok", text: savedText(view, t) });
      setRestartPending((view.restart_required ?? []).length > 0);
    } catch (error) { setNote({ kind: "error", text: t("common.notSaved", { message: (error as Error).message }) }); }
    finally { setSaving(false); }
  };

  return <section className="page" aria-labelledby="tools-title">
    <header className="page-header"><div className="page-title"><h2 id="tools-title">{t("tools.title")}</h2><p>{t("tools.blurb")}</p></div><div className="actions">
      <button type="button" className="btn ghost" onClick={() => void load()} disabled={saving}><Icon name="refresh" />{t("common.refresh")}</button><RestartButton className="btn danger" />
    </div></header>
    <form id="tools-form" autoComplete="off" onSubmit={submit}>
      <fieldset id="tools-fields" className="settings-fieldset section-stack" disabled={disabled}>
        <SettingsCard icon="compose" title={t("tools.shellTitle")} help={t("tools.shellHelp")}>
          <Row label={t("tools.execTool")} help={t("tools.execHelp")} htmlFor="set-exec"><Switch id="set-exec" label={t("tools.execTool")} checked={draft?.tools.exec ?? false} onChange={(value) => tool("exec", value)} /></Row>
          <Row label={t("tools.execSandbox")} htmlFor="set-exec-sandbox"><select id="set-exec-sandbox" className="select" value={draft?.tools.exec_sandbox ?? ""} onChange={(event) => tool("exec_sandbox", event.target.value)}>{draft && withCurrent(choices.exec_sandbox, draft.tools.exec_sandbox).map((value) => <option key={value} value={value}>{value || t("common.none")}</option>)}</select></Row>
        </SettingsCard>
        <SettingsCard icon="cloud" title={t("tools.webTitle")} help={t("tools.webHelp")} meta={<span className="chip hot">{t("tools.ssrfHot")}</span>}>
          <Row label={t("tools.webFetchTool")} help={t("tools.webFetchHelp")} htmlFor="set-web-fetch"><Switch id="set-web-fetch" label={t("tools.webFetchTool")} checked={draft?.tools.web_fetch ?? false} onChange={(value) => tool("web_fetch", value)} /></Row>
          <Row label={t("tools.browserTool")} help={t("tools.browserHelp")} htmlFor="set-browser"><Switch id="set-browser" label={t("tools.browserTool")} checked={draft?.tools.browser ?? false} onChange={(value) => tool("browser", value)} /></Row>
          <Row label={t("tools.ssrfWhitelist")} help={t("tools.ssrfWhitelistHint")} htmlFor="set-ssrf-whitelist" top><TagInput id="set-ssrf-whitelist" value={whitelistLines(draft?.tools.ssrf_whitelist ?? null)} onChange={(value) => edit((settings) => ({ ...settings, tools: { ...settings.tools, ssrf_whitelist: value } }))} placeholder={t("tools.ssrfPlaceholder")} removeLabel={(entry) => t("tools.ssrfRemove", { entry })} disabled={disabled} /></Row>
        </SettingsCard>
        <SettingsCard icon="search" title={t("tools.searchTitle")} help={t("tools.searchHelp")}>
          {ws && <><Row label={t("tools.webSearchProvider")} htmlFor="set-web-search-provider"><select id="set-web-search-provider" className="select" value={ws.provider} onChange={(event) => search("provider", event.target.value)}>{withCurrent(choices.web_provider ?? null, ws.provider).map((value) => <option key={value} value={value}>{value || t("common.none")}</option>)}</select></Row>
            {ws.provider && <Row label={t("tools.webSearchLimits")} htmlFor="set-web-search-max"><input id="set-web-search-max" className="input num" type="number" required min={1} max={50} step={1} title={t("tools.webSearchMax")} aria-label={t("tools.webSearchMax")} value={ws.max_results} onChange={(event) => search("max_results", event.target.valueAsNumber)} /><input id="set-web-search-timeout" className="input num" type="number" required min={1} max={300} step={1} title={t("tools.webSearchTimeout")} aria-label={t("tools.webSearchTimeout")} value={ws.timeout} onChange={(event) => search("timeout", event.target.valueAsNumber)} /></Row>}
            <Row label={t("tools.webFetchProvider")} htmlFor="set-web-fetch-provider" top help={<>{ws.key_source ? t("tools.webSearchKeySet", { source: ws.key_source }) : t("tools.webSearchKeyMissing")} <a href="#/providers">{t("tools.imageKeyLink")}</a>{t("tools.webSearchKeyAfter")}</>}><select id="set-web-fetch-provider" className="select" value={draft?.tools.web_fetch_provider ?? ""} onChange={(event) => edit((settings) => ({ ...settings, tools: { ...settings.tools, web_fetch_provider: event.target.value } }))}>{withCurrent(choices.web_provider ?? null, draft?.tools.web_fetch_provider ?? "").map((value) => <option key={value} value={value}>{value || t("tools.webFetchDirect")}</option>)}</select></Row></>}
        </SettingsCard>
        <SettingsCard icon="sparkle" title={t("tools.imageTitle")} help={<>{t("tools.imageKeyBefore")} <a href="#/providers">{t("tools.imageKeyLink")}</a>{t("tools.imageKeyAfter")}</>} meta={<Switch id="set-image-enabled" label={t("tools.imageGeneration")} checked={ig?.enabled ?? false} onChange={(value) => image("enabled", value)} />}>
          {ig?.enabled && <div id="set-image-generation" className="form-sub">
          <Row label={t("tools.imageProvider")} htmlFor="set-image-provider"><select id="set-image-provider" className="select" value={ig.provider} onChange={(event) => image("provider", event.target.value)}>{withCurrent(choices.image_generation_provider, ig.provider).map((value) => <option key={value} value={value}>{value || t("common.none")}</option>)}</select></Row>
          <Row label={t("tools.imageModel")} htmlFor="set-image-model"><input id="set-image-model" className="input mono wide" type="text" required value={ig.model} onChange={(event) => image("model", event.target.value)} /></Row>
          <Row label={t("tools.imageAspectRatio")} htmlFor="set-image-aspect"><input id="set-image-aspect" className="input num" type="text" required placeholder="1:1" value={ig.default_aspect_ratio} onChange={(event) => image("default_aspect_ratio", event.target.value)} /></Row>
          <Row label={t("tools.imageSize")} htmlFor="set-image-size"><input id="set-image-size" className="input num" type="text" required placeholder="1K" value={ig.default_image_size} onChange={(event) => image("default_image_size", event.target.value)} /></Row>
          <Row label={t("tools.imageMax")} htmlFor="set-image-max"><input id="set-image-max" className="input num" type="number" required min={1} max={8} step={1} value={ig.max_images_per_turn} onChange={(event) => image("max_images_per_turn", event.target.valueAsNumber)} /></Row>
          </div>}
        </SettingsCard>
      </fieldset>
      <div className="save-bar"><p id="tools-message" className="status-note" role="status" data-kind={note.kind || undefined}>{note.text || t("tools.saveNote")}</p><div className="actions">
        <button type="button" className="btn ghost" disabled={disabled || !dirty} onClick={() => setDraft(saved)}>{t("tools.discard")}</button><button type="submit" className="btn primary" disabled={disabled || !dirty}>{t("tools.saveChanges")}</button>
      </div></div>
      {restartPending && <RestartGateway />}
      <p className="status-note">{t("tools.savedNote")}</p>
    </form>
  </section>;
}
