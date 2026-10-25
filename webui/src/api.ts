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

import { signInRequired } from "./auth";

// requestError is the reason a response failed: its text, or the status.
// A 401 also shows the sign-in form.
async function requestError(resp: Response): Promise<Error> {
  if (resp.status === 401) signInRequired();
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

// POST /ui/upload saves a file to the workspace's uploads/ directory and
// answers where: path is what the agent's file tools take, url is where
// the page shows it.
export interface Upload {
  path: string;
  url: string;
  name: string;
  kind: "image" | "file";
}

export async function uploadFile(file: File): Promise<Upload> {
  const form = new FormData();
  form.append("file", file);
  const resp = await fetch("upload", { method: "POST", body: form });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
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

// GET /ui/cron lists jobs; POST creates; PATCH /ui/cron/{id} edits; POST …/run; DELETE removes.
export interface CronJob {
  id: string;
  name: string;
  enabled: boolean;
  system: boolean;
  schedule: {
    kind: string;
    at?: string;
    everyMs?: number;
    expr?: string;
    tz?: string;
    summary: string;
  };
  payload: { kind: string; message?: string };
  binding?: { channel: string; chatId: string };
  state: {
    nextRun?: string;
    lastRun?: string;
    lastStatus?: string;
    lastError?: string;
    runCount?: number;
  };
  createdAt?: string;
  updatedAt?: string;
  deleteAfterRun?: boolean;
}

export async function listCronJobs(): Promise<CronJob[]> {
  const resp = await fetch("cron", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  return ((await resp.json()) as { jobs?: CronJob[] }).jobs ?? [];
}

export async function runCronJob(id: string, force = false): Promise<CronJob> {
  const resp = await fetch("cron/" + encodeURIComponent(id) + "/run", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ force }),
  });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

export async function removeCronJob(id: string): Promise<void> {
  const resp = await fetch("cron/" + encodeURIComponent(id), { method: "DELETE" });
  if (!resp.ok) throw await requestError(resp);
}

/** Fields for POST /ui/cron and PATCH /ui/cron/{id}. */
export interface CronWrite {
  name?: string;
  message?: string;
  enabled?: boolean;
  deleteAfterRun?: boolean;
  schedule?: {
    kind: "at" | "every" | "cron" | string;
    at?: string;
    everyMs?: number;
    expr?: string;
    tz?: string;
  };
  binding?: { channel: string; chatId: string };
}

export async function createCronJob(body: CronWrite): Promise<CronJob> {
  const resp = await fetch("cron", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

export async function patchCronJob(id: string, body: CronWrite): Promise<CronJob> {
  const resp = await fetch("cron/" + encodeURIComponent(id), {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

// GET /ui/pairing; POST approve/deny/revoke.
export interface PairingPending {
  code: string;
  channel: string;
  sender_id: string;
  created_at: string;
  expires_at: string;
}

export interface PairingView {
  pending: PairingPending[];
  approved: Record<string, string[]>;
}

export async function getPairing(): Promise<PairingView> {
  const resp = await fetch("pairing", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  const v = (await resp.json()) as PairingView;
  return { pending: v.pending ?? [], approved: v.approved ?? {} };
}

export async function approvePairing(code: string): Promise<void> {
  const resp = await fetch("pairing/approve", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ code }),
  });
  if (!resp.ok) throw await requestError(resp);
}

export async function denyPairing(code: string): Promise<void> {
  const resp = await fetch("pairing/deny", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ code }),
  });
  if (!resp.ok) throw await requestError(resp);
}

export async function revokePairing(channel: string, senderId: string): Promise<void> {
  const resp = await fetch("pairing/revoke", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ channel, sender_id: senderId }),
  });
  if (!resp.ok) throw await requestError(resp);
}

// GET /ui/skills
export interface Skill {
  name: string;
  description?: string;
  path: string;
}

export async function listSkills(): Promise<Skill[]> {
  const resp = await fetch("skills", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  return ((await resp.json()) as { skills?: Skill[] }).skills ?? [];
}
