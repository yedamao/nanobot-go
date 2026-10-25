import type { Settings, SettingsPatch, SettingsView } from "./api";

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
  if (Object.keys(tools).length) patch.tools = tools;
  const channels: Record<string, { enabled: boolean }> = {};
  for (const [name, ch] of Object.entries(draft.channels)) {
    if (saved.channels[name] && ch.enabled !== saved.channels[name].enabled) channels[name] = { enabled: ch.enabled };
  }
  if (Object.keys(channels).length) patch.channels = channels;
  return patch;
}

// pendingText says which saved settings wait for a restart and which an
// environment variable overrides.
export function pendingText(view: SettingsView): string {
  const parts: string[] = [];
  const restart = view.restart_required ?? [];
  const env = view.env_overrides ?? [];
  if (restart.length) parts.push("Restart the gateway to apply: " + restart.join(", ") + ".");
  if (env.length) parts.push("Set by an environment variable, which wins over the saved value: " + env.join(", ") + ".");
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
