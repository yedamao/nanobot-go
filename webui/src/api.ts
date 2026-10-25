// The gateway's /ui/ HTTP endpoints, fetched relative to the page (/ui/).

// GET /ui/chats lists the stored chats; PATCH /ui/chats/{chat_id}
// {"title":"..."} renames one (answered with its entry) and DELETE
// /ui/chats/{chat_id} deletes it (204).
export interface ChatEntry {
  chat_id: string;
  updated_at?: string;
  title?: string;
  preview?: string;
}

// requestError is the reason a response failed: its text, or the status.
async function requestError(resp: Response): Promise<Error> {
  return new Error((await resp.text()).trim() || "HTTP " + resp.status);
}

export async function listChats(): Promise<ChatEntry[]> {
  const resp = await fetch("chats", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  return ((await resp.json()) as { chats?: ChatEntry[] }).chats ?? [];
}

async function chatRequest(id: string, method: string, body?: unknown): Promise<void> {
  const resp = await fetch("chats/" + encodeURIComponent(id), {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!resp.ok) throw await requestError(resp);
}

export function renameChat(id: string, title: string): Promise<void> {
  return chatRequest(id, "PATCH", { title });
}

export function deleteChat(id: string): Promise<void> {
  return chatRequest(id, "DELETE");
}

// GET /ui/status is what the gateway runs with now.
export interface Status {
  version: string;
  go?: string;
  started_at?: string;
  uptime_seconds: number;
  model: string;
  provider: string;
  workspace: string;
  channels: { name: string; enabled: boolean; running: boolean }[] | null;
  api_enabled: boolean;
  tools: { exec: boolean; web_fetch: boolean };
}

export async function getStatus(): Promise<Status> {
  const resp = await fetch("status", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

// GET and PATCH /ui/settings (PATCH takes only the changed fields of
// "settings") both answer a SettingsView. Saved settings apply after a
// gateway restart.
export interface Settings {
  model: string;
  provider: string;
  tools: { exec: boolean; web_fetch: boolean; exec_sandbox: string };
  channels: Record<string, { enabled: boolean }>;
}

export interface SettingsView {
  settings: Settings;
  restart_required: string[] | null;
  env_overrides: string[] | null;
  choices: { provider: string[] | null; exec_sandbox: string[] | null };
}

export interface SettingsPatch {
  model?: string;
  provider?: string;
  tools?: Partial<Settings["tools"]>;
  channels?: Record<string, { enabled: boolean }>;
}

export async function getSettings(): Promise<SettingsView> {
  const resp = await fetch("settings", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

export async function saveSettings(patch: SettingsPatch): Promise<SettingsView> {
  const resp = await fetch("settings", {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}
