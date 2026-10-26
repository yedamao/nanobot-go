import type { Provider, ProviderPatch, ProviderTest, ProviderTestResult } from "./api.ts";
import { enT, type Invalid, type Translator } from "./i18n/translate.ts";

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

/** checkProvider returns why draft cannot be saved, or null. */
export function checkProvider(d: ProviderDraft): Invalid | null {
  const key = d.apiKey.trim();
  if (!d.clearKey && key) {
    if (key.includes("***") || key.includes("•")) return { key: "providers.validation.keyRedacted" };
    if (key.length > maxAPIKeyLen) return { key: "providers.validation.keyTooLong", vars: { max: maxAPIKeyLen } };
    if (/[\s\p{Cc}]/u.test(key)) return { key: "providers.validation.keySpaces" };
  }
  const base = d.apiBase.trim();
  if (base) {
    if (base.length > maxAPIBaseLen) return { key: "providers.validation.baseTooLong", vars: { max: maxAPIBaseLen } };
    let u: URL;
    try {
      u = new URL(base);
    } catch {
      return { key: "providers.validation.baseUrl" };
    }
    if ((u.protocol !== "http:" && u.protocol !== "https:") || !u.host || /\s/.test(base)) {
      return { key: "providers.validation.baseUrl" };
    }
    if (u.username || u.password) return { key: "providers.validation.baseCredentials" };
  }
  return null;
}

/** keyStatus describes the saved key without revealing it. */
export function keyStatus(p: Provider, t: Translator = enT): string {
  const status = !p.hasApiKey
    ? t("common.notSet")
    : p.apiKeyHint
      ? t("common.setHint", { hint: p.apiKeyHint })
      : t("common.set");
  return p.env_overrides.includes("apiKey") ? t("common.envOverridden", { status }) : status;
}

/** canTest reports whether there is a key to test: one typed in, or a
 * saved (or environment) one while the form does not clear it. */
export function canTest(p: Provider, d: ProviderDraft): boolean {
  if (d.clearKey) return false;
  return d.apiKey.trim() !== "" || p.hasApiKey || p.env_overrides.includes("apiKey");
}

/** testBody is the POST /ui/providers/{name}/test body: the typed key if
 * any, else the saved one; the API base field only when it differs from
 * the saved one ("" then means the default endpoint). */
export function testBody(saved: Provider, d: ProviderDraft): ProviderTest {
  const body: ProviderTest = {};
  const key = d.apiKey.trim();
  if (key && !d.clearKey) body.apiKey = key;
  const base = d.apiBase.trim();
  if (base !== saved.apiBase) body.apiBase = base;
  return body;
}

/** testNote is the card's note for a probe's result. */
export function testNote(r: ProviderTestResult, t: Translator = enT): { kind: "ok" | "error"; text: string } {
  const message = r.message || t(r.ok ? "providers.credentialWorks" : "common.testFailedDefault");
  return { kind: r.ok ? "ok" : "error", text: t(r.ok ? "common.testPassed" : "common.testFailed", { message }) };
}
