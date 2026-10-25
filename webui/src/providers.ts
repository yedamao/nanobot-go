import type { Provider, ProviderPatch } from "./api.ts";

/** One provider's form. apiKey is a new key typed in, never the saved
 * one (the page is never sent it); "" keeps the saved key. */
export interface ProviderDraft {
  apiKey: string;
  clearKey: boolean;
  apiBase: string;
}

const maxAPIKeyLen = 4096;
const maxAPIBaseLen = 2048;

/** providerDraft is the untouched form for p. */
export function providerDraft(p: Provider): ProviderDraft {
  return { apiKey: "", clearKey: false, apiBase: p.apiBase };
}

/** diffProvider returns the PATCH body for what draft changes. */
export function diffProvider(saved: Provider, d: ProviderDraft): ProviderPatch {
  const patch: ProviderPatch = {};
  const key = d.apiKey.trim();
  if (d.clearKey) {
    if (saved.hasApiKey) patch.clearApiKey = true;
  } else if (key) {
    patch.apiKey = key;
  }
  const base = d.apiBase.trim();
  if (base !== saved.apiBase) patch.apiBase = base;
  return patch;
}

/** checkProvider returns why draft cannot be saved, or "". */
export function checkProvider(d: ProviderDraft): string {
  const key = d.apiKey.trim();
  if (!d.clearKey && key) {
    if (key.includes("***") || key.includes("•")) return "API key is a redacted placeholder; paste the real key";
    if (key.length > maxAPIKeyLen) return "API key must be at most " + maxAPIKeyLen + " characters";
    if (/[\s\p{Cc}]/u.test(key)) return "API key must not contain spaces";
  }
  const base = d.apiBase.trim();
  if (base) {
    if (base.length > maxAPIBaseLen) return "API base must be at most " + maxAPIBaseLen + " characters";
    let u: URL;
    try {
      u = new URL(base);
    } catch {
      return "API base must be an http:// or https:// URL";
    }
    if ((u.protocol !== "http:" && u.protocol !== "https:") || !u.host || /\s/.test(base)) {
      return "API base must be an http:// or https:// URL";
    }
    if (u.username || u.password) return "API base must not hold credentials; set the API key instead";
  }
  return "";
}

/** keyStatus describes the saved key without revealing it. */
export function keyStatus(p: Provider): string {
  const env = p.env_overrides.includes("apiKey") ? " (overridden by the environment)" : "";
  if (!p.hasApiKey) return "Not set" + env;
  return "Set" + (p.apiKeyHint ? ": " + p.apiKeyHint : "") + env;
}
