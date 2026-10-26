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
export interface FallbackModel {
  provider: string;
  model: string;
}

export interface Settings {
  model: string;
  provider: string;
  /** Tried in order when the primary provider fails. */
  fallbackModels: FallbackModel[] | null;
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
  /** Replaces the whole list; [] clears it. */
  fallbackModels?: FallbackModel[];
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

// GET /ui/cron/{id}/runs is a job's recent runs (the last 20), newest first.
export interface CronRun {
  runAt: string;
  status: "ok" | "error" | "skipped" | string;
  durationMs?: number;
  error?: string;
}

export interface CronRuns {
  jobId: string;
  name: string;
  runs: CronRun[];
}

export async function listCronRuns(id: string): Promise<CronRuns> {
  const resp = await fetch("cron/" + encodeURIComponent(id) + "/runs", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  const body = (await resp.json()) as CronRuns;
  return { ...body, runs: body.runs ?? [] };
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
// GET /ui/skills lists workspace skills, then bundled builtins not yet
// installed. PATCH /ui/skills/{name} {"enabled":bool} enables or
// disables one, POST /ui/skills/install {"name":"..."} copies a builtin
// into the workspace (201) and DELETE /ui/skills/{name} removes a
// workspace skill (204).
export interface Skill {
  name: string;
  description?: string;
  source: "workspace" | "builtin";
  builtin: boolean;
  installed: boolean;
  enabled: boolean;
  /** Workspace-relative SKILL.md path; absent for an uninstalled builtin. */
  path?: string;
}

export interface SkillsList {
  skills: Skill[];
  /** Whether enable/disable is saved to the config. */
  persistent: boolean;
}

export async function listSkills(): Promise<SkillsList> {
  const resp = await fetch("skills", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  const body = (await resp.json()) as Partial<SkillsList>;
  return { skills: body.skills ?? [], persistent: !!body.persistent };
}

export async function setSkillEnabled(name: string, enabled: boolean): Promise<Skill> {
  const resp = await fetch("skills/" + encodeURIComponent(name), {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ enabled }),
  });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

export async function installSkill(name: string): Promise<Skill> {
  const resp = await fetch("skills/install", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name }),
  });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

export async function deleteSkill(name: string): Promise<void> {
  const resp = await fetch("skills/" + encodeURIComponent(name), { method: "DELETE" });
  if (!resp.ok) throw await requestError(resp);
}

// GET /ui/mcp lists tools.mcpServers merged with the running gateway's
// connection states. Env and header values are never sent, only their
// names. PUT /ui/mcp/{name} creates (201) or replaces (200) a server,
// PATCH /ui/mcp/{name} {"enabled":bool} turns one on or off and DELETE
// /ui/mcp/{name} removes it (204). MCP servers connect when the gateway
// starts: changes need a restart (restart_required).
export interface MCPServer {
  name: string;
  enabled: boolean;
  transport: string;
  type?: string;
  command?: string;
  args?: string[];
  cwd?: string;
  url?: string;
  env_keys?: string[];
  header_keys?: string[];
  tool_timeout?: number;
  enabled_tools: string[] | null;
  state: string;
  error?: string;
  tools?: string[];
  restart_required: boolean;
  removed?: boolean;
}

export interface MCPList {
  servers: MCPServer[];
  live: boolean;
  restart_required: string[];
}

/** The body of PUT /ui/mcp/{name}. */
export interface MCPServerInput {
  type?: string;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  cwd?: string;
  url?: string;
  headers?: Record<string, string>;
  toolTimeout?: number;
  enabledTools?: string[];
  enabled?: boolean;
}

export async function listMCP(): Promise<MCPList> {
  const resp = await fetch("mcp", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  const body = (await resp.json()) as Partial<MCPList>;
  return { servers: body.servers ?? [], live: !!body.live, restart_required: body.restart_required ?? [] };
}

async function mcpRequest(name: string, method: string, body?: unknown): Promise<Response> {
  const resp = await fetch("mcp/" + encodeURIComponent(name), {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!resp.ok) throw await requestError(resp);
  return resp;
}

export async function putMCPServer(name: string, input: MCPServerInput): Promise<MCPServer> {
  return (await mcpRequest(name, "PUT", input)).json();
}

export async function setMCPServerEnabled(name: string, enabled: boolean): Promise<MCPServer> {
  return (await mcpRequest(name, "PATCH", { enabled })).json();
}

export async function deleteMCPServer(name: string): Promise<void> {
  await mcpRequest(name, "DELETE");
}

// GET /ui/providers lists the known providers' credentials: never an
// apiKey, only hasApiKey and a short apiKeyHint (sk-a••••xyz1). PATCH
// /ui/providers/{name} sets (apiKey) or clears (clearApiKey) the key and
// sets apiBase ("" removes it). Provider clients are built when the
// gateway starts: changes need a restart (restart_required).
export interface Provider {
  name: string;
  hasApiKey: boolean;
  apiKeyHint: string;
  apiBase: string;
  defaultApiBase?: string;
  restart_required: boolean;
  /** "apiKey" and/or "apiBase" when NANOBOT_GO_<NAME>_API_KEY/_API_BASE win over the file. */
  env_overrides: string[];
}

/** The body of PATCH /ui/providers/{name}. */
export interface ProviderPatch {
  apiKey?: string;
  clearApiKey?: boolean;
  apiBase?: string;
}

export async function listProviders(): Promise<Provider[]> {
  const resp = await fetch("providers", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  return ((await resp.json()) as { providers?: Provider[] }).providers ?? [];
}

export async function saveProvider(name: string, patch: ProviderPatch): Promise<Provider> {
  const resp = await fetch("providers/" + encodeURIComponent(name), {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

// POST /ui/providers/{name}/test probes a credential with the provider's
// models list (no completion tokens). A missing apiKey/apiBase tests the
// saved one (environment applied); drafts are used once, never saved.
// A finished probe answers 200 whatever its outcome; the message never
// holds the key.
export interface ProviderTest {
  apiKey?: string;
  apiBase?: string;
}

export interface ProviderTestResult {
  ok: boolean;
  status: "ok" | "not_configured" | "rejected" | "error";
  message: string;
}

export async function testProvider(name: string, body: ProviderTest): Promise<ProviderTestResult> {
  const resp = await fetch("providers/" + encodeURIComponent(name) + "/test", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

// GET /ui/providers/{name}/models is an advisory model list for the
// Models page's picker ({name} is a provider or "auto"): the built-in
// list, or the provider's own /models when it is OpenAI-compatible and
// has a key. It never holds credentials; any model ID can still be typed.
export interface CatalogModel {
  id: string;
  label?: string;
}

export interface ProviderModels {
  provider: string;
  catalog_kind: "curated" | "fetched" | "unsupported";
  status: "available" | "not_configured" | "error" | "unsupported";
  message?: string;
  source: "curated" | "provider";
  models: CatalogModel[] | null;
}

export async function getProviderModels(name: string): Promise<ProviderModels> {
  const resp = await fetch("providers/" + encodeURIComponent(name) + "/models", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

// GET /ui/gateway says how the gateway serving this page was launched
// and whether the page can restart it: only a background gateway
// (`nanobot gateway --background`, or the on-demand one `nanobot webui`
// starts) can; a foreground one must be restarted in its terminal.
// POST /ui/gateway/restart answers 202 and the gateway restarts shortly
// after (409 with the reason when it cannot).
export interface GatewayInfo {
  pid: number;
  started_at?: string;
  launch_mode: string;
  lifetime: string;
  restart_available: boolean;
  restart_unavailable_reason?: string;
  restarting: boolean;
}

export async function getGateway(): Promise<GatewayInfo> {
  const resp = await fetch("gateway", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

export async function restartGateway(): Promise<void> {
  const resp = await fetch("gateway/restart", { method: "POST" });
  if (resp.status === 409) {
    const body = (await resp.json().catch(() => ({}))) as { error?: string };
    throw new Error(body.error || "The gateway cannot be restarted from here.");
  }
  if (!resp.ok) throw await requestError(resp);
}

// GET /ui/channels/config lists the channels the page can configure
// (telegram, weixin), present in config.json or not: never a token, only
// hasToken and a short tokenHint (123456:••••abcd). PATCH
// /ui/channels/config/{name} creates or changes the section; channels
// are built when the gateway starts, so changes need a restart
// (restart_required).
export interface ChannelConfig {
  name: string;
  present: boolean;
  enabled: boolean;
  hasToken: boolean;
  tokenHint: string;
  allowFrom: string[];
  /** Seconds; 0 uses the default. */
  pollTimeout: number;
  /** Telegram. */
  apiBase?: string;
  groupPolicy?: string;
  replyToMessage?: boolean;
  hasProxy?: boolean;
  /** WeChat. */
  baseUrl?: string;
  routeTag?: string;
  /** A token saved by `nanobot channels login weixin`. */
  hasLoginState?: boolean;
  /** WeChat: the saved login, without its secrets. */
  login?: WeixinLogin;
  defaultBase: string;
  /** "token" when NANOBOT_GO_<CHANNEL>_TOKEN wins over the file. */
  env_overrides: string[];
  restart_required: boolean;
  running: boolean;
  testSupported: boolean;
}

/** The login saved by `nanobot channels login weixin`: never its token. */
export interface WeixinLogin {
  loggedIn: boolean;
  botId?: string;
  userId?: string;
  /** The API host the login assigned. */
  baseUrl?: string;
  statePath: string;
  /** Why the saved login cannot be read (never its contents). */
  error?: string;
  /** The shell command that logs in, with --config when not the default. */
  cliCommand: string;
  loginHint: string;
}

/** The body of PATCH /ui/channels/config/{name}. */
export interface ChannelConfigPatch {
  enabled?: boolean;
  token?: string;
  clearToken?: boolean;
  allowFrom?: string[];
  pollTimeout?: number;
  apiBase?: string;
  groupPolicy?: string;
  replyToMessage?: boolean;
  baseUrl?: string;
  routeTag?: string;
}

export interface ChannelTestResult {
  ok: boolean;
  status: "ok" | "not_configured" | "rejected" | "error" | "unsupported";
  message: string;
}

export async function listChannelConfigs(): Promise<ChannelConfig[]> {
  const resp = await fetch("channels/config", { cache: "no-store" });
  if (!resp.ok) throw await requestError(resp);
  return ((await resp.json()) as { channels?: ChannelConfig[] }).channels ?? [];
}

export async function saveChannelConfig(name: string, patch: ChannelConfigPatch): Promise<ChannelConfig> {
  const resp = await fetch("channels/config/" + encodeURIComponent(name), {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(patch),
  });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

// DELETE /ui/channels/config/{name}/login removes the login saved by
// `nanobot channels login weixin` (account.json only, never config.json)
// and returns the channel afterwards. A running channel keeps the login
// until the gateway restarts (restart_required).
export async function clearChannelLogin(name: string): Promise<ChannelConfig> {
  const resp = await fetch("channels/config/" + encodeURIComponent(name) + "/login", { method: "DELETE" });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}

// POST /ui/channels/config/{name}/test checks a token (Telegram: getMe)
// with the typed token, else the saved one; the draft is never saved.
export async function testChannelConfig(name: string, token?: string): Promise<ChannelTestResult> {
  const resp = await fetch("channels/config/" + encodeURIComponent(name) + "/test", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(token ? { token } : {}),
  });
  if (!resp.ok) throw await requestError(resp);
  return resp.json();
}
