import type { ChannelConfig, ChannelConfigPatch, ChannelTestResult } from "./api.ts";

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

/** checkToken returns why a typed token cannot be used, or "". */
export function checkToken(name: string, raw: string): string {
  const token = raw.trim();
  if (!token) return "";
  if (token.includes("***") || token.includes("•")) return "token is a redacted placeholder; paste the real token";
  if (token.length > maxTokenLen) return "token must be at most " + maxTokenLen + " characters";
  if (/[\s\p{Cc}]/u.test(token)) return "token must not contain spaces";
  if (name === "telegram" && !telegramToken.test(token)) return "token must look like 123456:ABC… as issued by @BotFather";
  return "";
}

function checkURL(label: string, raw: string): string {
  const s = raw.trim();
  if (!s) return "";
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    return label + " must be an http:// or https:// URL";
  }
  if ((u.protocol !== "http:" && u.protocol !== "https:") || !u.host || /\s/.test(s)) {
    return label + " must be an http:// or https:// URL";
  }
  if (u.username || u.password) return label + " must not hold credentials";
  return "";
}

/** checkChannel returns why draft cannot be saved, or "". */
export function checkChannel(saved: ChannelConfig, d: ChannelDraft): string {
  if (!d.clearToken) {
    const bad = checkToken(saved.name, d.token);
    if (bad) return bad;
  }
  const poll = d.pollTimeout.trim();
  if (poll && !(/^\d+$/.test(poll) && Number(poll) <= 3600)) return "poll timeout must be a whole number of seconds up to 3600";
  if (saved.name === "telegram") {
    const bad = checkURL("API base", d.apiBase);
    if (bad) return bad;
    if (d.enabled && !hasTokenAfter(saved, d)) return "Telegram needs a bot token to be enabled";
  }
  if (saved.name === "weixin") {
    const bad = checkURL("base URL", d.baseUrl);
    if (bad) return bad;
  }
  return "";
}

/** hasTokenAfter reports whether a token would be there after saving
 * draft: typed, kept or from the environment. */
function hasTokenAfter(saved: ChannelConfig, d: ChannelDraft): boolean {
  if (saved.env_overrides.includes("token")) return true;
  if (d.clearToken) return false;
  return d.token.trim() !== "" || saved.hasToken;
}

/** tokenStatus describes the saved token without revealing it. */
export function tokenStatus(c: ChannelConfig): string {
  const env = c.env_overrides.includes("token") ? " (overridden by the environment)" : "";
  if (c.hasToken) return "Set" + (c.tokenHint ? ": " + c.tokenHint : "") + env;
  if (c.name === "weixin" && c.hasLoginState) return "From 'nanobot channels login weixin'" + env;
  return "Not set" + env;
}

/** channelState is the one-line state of a channel. */
export function channelState(c: ChannelConfig): string {
  if (!c.present) return "not in config.json";
  if (c.running) return "running";
  return c.enabled ? "enabled, not running" : "disabled";
}

/** canTest reports whether Test has a token to probe. */
export function canTest(c: ChannelConfig, d: ChannelDraft): boolean {
  if (!c.testSupported || d.clearToken) return false;
  return d.token.trim() !== "" || c.hasToken || c.env_overrides.includes("token");
}

/** testNote is the card's note for a probe's result. */
export function testNote(r: ChannelTestResult): { kind: "ok" | "error" | ""; text: string } {
  if (r.status === "unsupported") return { kind: "", text: r.message };
  const text = r.message || (r.ok ? "The token works." : "The test failed.");
  return { kind: r.ok ? "ok" : "error", text: (r.ok ? "Test passed: " : "Test failed: ") + text };
}
