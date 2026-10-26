import type { ChannelConfig, ChannelConfigPatch, ChannelTestResult, WeixinLogin } from "./api.ts";
import { enT, type Invalid, type Translator } from "./i18n/translate.ts";

/** One channel's form. token is a new token typed in, never the saved one
 * (the page is never sent it); "" keeps the saved token. allowFrom is
 * the comma-separated list as typed. */
export interface ChannelDraft {
  enabled: boolean;
  token: string;
  clearToken: boolean;
  allowFrom: string;
  pollTimeout: string;
  apiBase: string;
  groupPolicy: string;
  replyToMessage: boolean;
  baseUrl: string;
  routeTag: string;
}

const maxTokenLen = 4096;
const telegramToken = /^[0-9]+:[A-Za-z0-9_-]+$/;

/** channelDraft is the untouched form for c. */
export function channelDraft(c: ChannelConfig): ChannelDraft {
  return {
    enabled: c.enabled,
    token: "",
    clearToken: false,
    allowFrom: c.allowFrom.join(", "),
    pollTimeout: c.pollTimeout ? String(c.pollTimeout) : "",
    apiBase: c.apiBase ?? "",
    groupPolicy: c.groupPolicy ?? "",
    replyToMessage: c.replyToMessage ?? false,
    baseUrl: c.baseUrl ?? "",
    routeTag: c.routeTag ?? "",
  };
}

/** parseAllowFrom splits a comma- or space-separated list, dropping
 * empty and repeated entries. */
export function parseAllowFrom(s: string): string[] {
  const out: string[] = [];
  for (const e of s.split(/[,\s]+/)) {
    if (e && !out.includes(e)) out.push(e);
  }
  return out;
}

function sameList(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

/** diffChannel returns the PATCH body for what draft changes. A channel
 * missing from config.json is created with its enabled flag. */
export function diffChannel(saved: ChannelConfig, d: ChannelDraft): ChannelConfigPatch {
  const patch: ChannelConfigPatch = {};
  if (d.enabled !== saved.enabled || !saved.present) patch.enabled = d.enabled;
  const token = d.token.trim();
  if (d.clearToken) {
    if (saved.hasToken) patch.clearToken = true;
  } else if (token) {
    patch.token = token;
  }
  const allow = parseAllowFrom(d.allowFrom);
  if (!sameList(allow, saved.allowFrom)) patch.allowFrom = allow;
  const poll = d.pollTimeout.trim() === "" ? 0 : Number(d.pollTimeout.trim());
  if (poll !== saved.pollTimeout) patch.pollTimeout = poll;
  if (saved.name === "telegram") {
    if (d.apiBase.trim() !== (saved.apiBase ?? "")) patch.apiBase = d.apiBase.trim();
    if (d.groupPolicy !== (saved.groupPolicy ?? "")) patch.groupPolicy = d.groupPolicy;
    if (d.replyToMessage !== (saved.replyToMessage ?? false)) patch.replyToMessage = d.replyToMessage;
  } else if (saved.name === "weixin") {
    if (d.baseUrl.trim() !== (saved.baseUrl ?? "")) patch.baseUrl = d.baseUrl.trim();
    if (d.routeTag.trim() !== (saved.routeTag ?? "")) patch.routeTag = d.routeTag.trim();
  }
  return patch;
}

/** checkToken returns why a typed token cannot be used, or null. */
export function checkToken(name: string, raw: string): Invalid | null {
  const token = raw.trim();
  if (!token) return null;
  if (token.includes("***") || token.includes("•")) return { key: "channels.validation.tokenRedacted" };
  if (token.length > maxTokenLen) return { key: "channels.validation.tokenTooLong", vars: { max: maxTokenLen } };
  if (/[\s\p{Cc}]/u.test(token)) return { key: "channels.validation.tokenSpaces" };
  if (name === "telegram" && !telegramToken.test(token)) return { key: "channels.validation.telegramTokenShape" };
  return null;
}

/** urlProblem says what is wrong with an optional http(s) URL field. */
function urlProblem(raw: string): "" | "scheme" | "credentials" {
  const s = raw.trim();
  if (!s) return "";
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    return "scheme";
  }
  if ((u.protocol !== "http:" && u.protocol !== "https:") || !u.host || /\s/.test(s)) return "scheme";
  if (u.username || u.password) return "credentials";
  return "";
}

/** checkChannel returns why draft cannot be saved, or null. */
export function checkChannel(saved: ChannelConfig, d: ChannelDraft): Invalid | null {
  if (!d.clearToken) {
    const bad = checkToken(saved.name, d.token);
    if (bad) return bad;
  }
  const poll = d.pollTimeout.trim();
  if (poll && !(/^\d+$/.test(poll) && Number(poll) <= 3600)) return { key: "channels.validation.pollTimeout", vars: { max: 3600 } };
  if (saved.name === "telegram") {
    const bad = urlProblem(d.apiBase);
    if (bad === "scheme") return { key: "channels.validation.apiBaseUrl" };
    if (bad === "credentials") return { key: "channels.validation.apiBaseCredentials" };
    if (d.enabled && !hasTokenAfter(saved, d)) return { key: "channels.validation.telegramNeedsToken" };
  }
  if (saved.name === "weixin") {
    const bad = urlProblem(d.baseUrl);
    if (bad === "scheme") return { key: "channels.validation.baseUrlUrl" };
    if (bad === "credentials") return { key: "channels.validation.baseUrlCredentials" };
  }
  return null;
}

/** hasTokenAfter reports whether a token would be there after saving
 * draft: typed, kept or from the environment. */
function hasTokenAfter(saved: ChannelConfig, d: ChannelDraft): boolean {
  if (saved.env_overrides.includes("token")) return true;
  if (d.clearToken) return false;
  return d.token.trim() !== "" || saved.hasToken;
}

/** tokenStatus describes the saved token without revealing it. */
export function tokenStatus(c: ChannelConfig, t: Translator = enT): string {
  const env = c.env_overrides.includes("token");
  if (c.hasToken) return withEnv(c.tokenHint ? t("common.setHint", { hint: c.tokenHint }) : t("common.set"), env, t);
  if (c.name === "weixin" && c.hasLoginState) return withEnv(t("channels.tokenFromLogin"), env, t);
  return withEnv(t("common.notSet"), env, t);
}

function withEnv(status: string, env: boolean, t: Translator): string {
  return env ? t("common.envOverridden", { status }) : status;
}


/** channelState is the one-line state of a channel. */
export function channelState(c: ChannelConfig, t: Translator = enT): string {
  if (!c.present) return t("channels.notInConfig");
  if (c.running) return t("common.running");
  return t(c.enabled ? "common.enabledNotRunning" : "common.disabled");
}

/** canTest reports whether Test has a token to probe. */
export function canTest(c: ChannelConfig, d: ChannelDraft): boolean {
  if (!c.testSupported || d.clearToken) return false;
  return d.token.trim() !== "" || c.hasToken || c.env_overrides.includes("token");
}

/** testNote is the card's note for a probe's result. */
export function testNote(r: ChannelTestResult, t: Translator = enT): { kind: "ok" | "error" | ""; text: string } {
  if (r.status === "unsupported") return { kind: "", text: r.message };
  const message = r.message || t(r.ok ? "channels.tokenWorks" : "common.testFailedDefault");
  return { kind: r.ok ? "ok" : "error", text: t(r.ok ? "common.testPassed" : "common.testFailed", { message }) };
}

/** loginStatus is the WeChat card's one-line login state. */
export function loginStatus(l: WeixinLogin, t: Translator = enT): { kind: "ok" | "error" | ""; text: string } {
  if (l.error) return { kind: "error", text: t("channels.login.unreadable", { error: l.error }) };
  if (l.loggedIn) return { kind: "ok", text: t("channels.login.saved") };
  return { kind: "", text: t("channels.login.notLoggedIn") };
}

/** loginDetails are the saved login's non-secret fields to show. */
export function loginDetails(l: WeixinLogin, t: Translator = enT): [string, string][] {
  if (!l.loggedIn) return [];
  const out: [string, string][] = [];
  if (l.botId) out.push([t("channels.login.botId"), l.botId]);
  if (l.userId) out.push([t("channels.login.userId"), l.userId]);
  if (l.baseUrl) out.push([t("channels.login.baseUrl"), l.baseUrl]);
  return out;
}

/** forceLoginCommand is cmd logging in again over a saved login. */
export function forceLoginCommand(cmd: string): string {
  return cmd + " --force";
}

/** weixinLogin picks WeChat's login out of a channel config list. */
/** canClearLogin reports a saved login file to remove: a login, or one
 * that cannot be read. */
export function canClearLogin(l: WeixinLogin): boolean {
  return l.loggedIn || !!l.error;
}

/** clearLoginPrompt asks before the saved login is removed. */
export function clearLoginPrompt(l: WeixinLogin, running: boolean, t: Translator = enT): string {
  const msg = t("channels.login.clearConfirm", { path: l.statePath });
  return running ? msg + t("channels.login.clearConfirmRunning") : msg;
}

/** withLogin is c with the login of v, a view returned after the login
 * changed; the section's saved settings are kept as they are. */
export function withLogin(c: ChannelConfig, v: ChannelConfig): ChannelConfig {
  return { ...c, login: v.login, hasLoginState: v.hasLoginState, restart_required: v.restart_required, running: v.running };
}

export function weixinLogin(list: ChannelConfig[]): WeixinLogin | undefined {
  return list.find((c) => c.name === "weixin")?.login;
}
