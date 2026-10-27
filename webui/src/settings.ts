import type { ImageGenerationSettings, Settings, SettingsPatch, SettingsView, WebSearchSettings } from "./api";
import { enT, type Translator } from "./i18n/translate.ts";

// diffSettings returns the fields of draft that differ from saved, as a
// PATCH /ui/settings body. The model is compared trimmed.
export function diffSettings(saved: Settings, draft: Settings): SettingsPatch {
  const patch: SettingsPatch = {};
  const model = draft.model.trim();
  if (model !== saved.model) patch.model = model;
  if (draft.provider !== saved.provider) patch.provider = draft.provider;
  const tools: SettingsPatch["tools"] = {};
  if (draft.tools.exec !== saved.tools.exec) tools.exec = draft.tools.exec;
  if (draft.tools.web_fetch !== saved.tools.web_fetch) tools.web_fetch = draft.tools.web_fetch;
  if (draft.tools.exec_sandbox !== saved.tools.exec_sandbox) tools.exec_sandbox = draft.tools.exec_sandbox;
  const image = diffImageGeneration(saved.tools.image_generation, draft.tools.image_generation);
  if (Object.keys(image).length) tools.image_generation = image;
  if (saved.tools.web_search && draft.tools.web_search) {
    const search = diffWebSearch(saved.tools.web_search, draft.tools.web_search);
    if (Object.keys(search).length) tools.web_search = search;
  }
  const fetchProvider = draft.tools.web_fetch_provider ?? "";
  if (fetchProvider !== (saved.tools.web_fetch_provider ?? "")) tools.web_fetch_provider = fetchProvider;
  const whitelist = whitelistLines(draft.tools.ssrf_whitelist);
  const savedWhitelist = saved.tools.ssrf_whitelist ?? [];
  if (whitelist.length !== savedWhitelist.length || whitelist.some((e, i) => e !== savedWhitelist[i])) {
    tools.ssrf_whitelist = whitelist;
  }
  if (Object.keys(tools).length) patch.tools = tools;
  const channels: Record<string, { enabled: boolean }> = {};
  for (const [name, ch] of Object.entries(draft.channels)) {
    if (saved.channels[name] && ch.enabled !== saved.channels[name].enabled) channels[name] = { enabled: ch.enabled };
  }
  if (Object.keys(channels).length) patch.channels = channels;
  return patch;
}

// diffImageGeneration returns the image generation fields of draft that
// differ from saved; text fields are compared trimmed.
function diffImageGeneration(saved: ImageGenerationSettings, draft: ImageGenerationSettings): Partial<ImageGenerationSettings> {
  const patch: Partial<ImageGenerationSettings> = {};
  if (draft.enabled !== saved.enabled) patch.enabled = draft.enabled;
  for (const key of ["provider", "model", "default_aspect_ratio", "default_image_size"] as const) {
    const v = draft[key].trim();
    if (v !== saved[key]) patch[key] = v;
  }
  if (draft.max_images_per_turn !== saved.max_images_per_turn) patch.max_images_per_turn = draft.max_images_per_turn;
  return patch;
}

// diffWebSearch returns the web search fields of draft that differ from
// saved; key_source is read-only and never sent.
function diffWebSearch(saved: WebSearchSettings, draft: WebSearchSettings): NonNullable<NonNullable<SettingsPatch["tools"]>["web_search"]> {
  const patch: NonNullable<NonNullable<SettingsPatch["tools"]>["web_search"]> = {};
  if (draft.provider !== saved.provider) patch.provider = draft.provider;
  if (draft.max_results !== saved.max_results) patch.max_results = draft.max_results;
  if (draft.timeout !== saved.timeout) patch.timeout = draft.timeout;
  return patch;
}

// whitelistLines returns the SSRF whitelist entries of a draft, which
// holds the textarea's lines as typed: trimmed, without blank lines.
export function whitelistLines(lines: string[] | null): string[] {
  return (lines ?? []).map((l) => l.trim()).filter((l) => l !== "");
}

// savedText is the note after a save: which settings the running gateway
// took on at once, and which wait for a restart.
export function savedText(view: SettingsView, t: Translator = enT): string {
  const pending = pendingText(view, t);
  const applied = view.applied ?? [];
  if (applied.length) return t("status.savedApplied", { names: applied.join(", "), pending }).trim();
  return t("status.saved", { pending }).trim();
}

// pendingText says which saved settings wait for a restart and which an
// environment variable overrides.
export function pendingText(view: SettingsView, t: Translator = enT): string {
  const parts: string[] = [];
  const restart = view.restart_required ?? [];
  const env = view.env_overrides ?? [];
  if (restart.length) parts.push(t("status.pendingRestart", { names: restart.join(", ") }));
  if (env.length) parts.push(t("status.pendingEnv", { names: env.join(", ") }));
  return parts.join(" ");
}

// withCurrent returns choices with current first when it is not among
// them (such as another system's sandbox), so a select can show it.
export function withCurrent(choices: string[] | null, current: string): string[] {
  const list = choices ?? [];
  return list.includes(current) ? list : [current, ...list];
}

// duration formats seconds as "3d 4h", "2h 5m", "7m 3s" or "9s".
export function duration(s: number): string {
  const d = Math.floor(s / 86400),
    h = Math.floor((s % 86400) / 3600),
    m = Math.floor((s % 3600) / 60);
  if (d) return d + "d " + h + "h";
  if (h) return h + "h " + m + "m";
  if (m) return m + "m " + (s % 60) + "s";
  return s + "s";
}
