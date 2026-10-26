import type { FallbackModel, ProviderModels, Settings, SettingsPatch } from "./api.ts";
import { enT, type Translator } from "./i18n/translate.ts";

/** The Models page's form: the primary model and the ordered fallbacks. */
export interface ModelsDraft {
  model: string;
  provider: string;
  fallbacks: FallbackModel[];
}

const maxModelLen = 200;
export const maxFallbacks = 16;

/** modelsDraft reads the form's fields from saved settings. */
export function modelsDraft(s: Settings): ModelsDraft {
  return { model: s.model, provider: s.provider, fallbacks: (s.fallbackModels ?? []).map((f) => ({ ...f })) };
}

function trimmed(list: FallbackModel[]): FallbackModel[] {
  return list.map((f) => ({ provider: f.provider || "auto", model: f.model.trim() }));
}

/** diffModels returns the PATCH /ui/settings body for what draft changes;
 * the fallback list is sent whole when any entry or the order differs. */
export function diffModels(saved: Settings, draft: ModelsDraft): SettingsPatch {
  const patch: SettingsPatch = {};
  const model = draft.model.trim();
  if (model !== saved.model) patch.model = model;
  if (draft.provider !== saved.provider) patch.provider = draft.provider;
  const next = trimmed(draft.fallbacks);
  const prev = saved.fallbackModels ?? [];
  if (next.length !== prev.length || next.some((f, i) => f.provider !== prev[i].provider || f.model !== prev[i].model)) {
    patch.fallbackModels = next;
  }
  return patch;
}

function modelError(name: string, m: string): string {
  const v = m.trim();
  if (!v) return name + " must not be empty";
  if (v.length > maxModelLen) return name + " must be at most " + maxModelLen + " characters";
  if (/\s/.test(v)) return name + " must not contain spaces";
  return "";
}

/** checkModels returns why draft cannot be saved, or "". */
export function checkModels(draft: ModelsDraft): string {
  const err = modelError("Model", draft.model);
  if (err) return err;
  if (draft.fallbacks.length > maxFallbacks) return "at most " + maxFallbacks + " fallbacks";
  for (const [i, f] of draft.fallbacks.entries()) {
    const e = modelError("Fallback " + (i + 1) + " model", f.model);
    if (e) return e;
  }
  return "";
}

/** moveItem returns list with item i moved by delta places; out of range
 * moves return list unchanged. */
export function moveItem<T>(list: T[], i: number, delta: number): T[] {
  const j = i + delta;
  if (i < 0 || i >= list.length || j < 0 || j >= list.length) return list;
  const out = list.slice();
  [out[i], out[j]] = [out[j], out[i]];
  return out;
}

/** A provider's catalog as the page holds it: loading, loaded, or why
 * it could not be loaded. */
export type Catalog = { state: "loading" } | { state: "ok"; models: ProviderModels } | { state: "error"; error: string };

/** draftProviders lists the providers draft's rows use, each once, in
 * order: the catalogs the page needs. */
export function draftProviders(draft: ModelsDraft): string[] {
  const out: string[] = [];
  for (const p of [draft.provider, ...draft.fallbacks.map((f) => f.provider)]) {
    const name = p || "auto";
    if (!out.includes(name)) out.push(name);
  }
  return out;
}

/** catalogListId is the <datalist> id holding provider's suggestions. */
export function catalogListId(provider: string): string {
  return "models-catalog-" + (provider || "auto").replace(/[^A-Za-z0-9_-]/g, "_");
}

/** catalogOptions are the suggestions a catalog offers, labelled for
 * the datalist. */
export function catalogOptions(c: Catalog | undefined): { value: string; label: string }[] {
  if (c?.state !== "ok") return [];
  return (c.models.models ?? []).map((m) => ({ value: m.id, label: m.label ?? "" }));
}

/** catalogNote is the quiet line under a model field: where its
 * suggestions come from, or why there are none. "" when nothing to say. */
export function catalogNote(c: Catalog | undefined, t: Translator = enT): string {
  if (!c) return "";
  if (c.state === "loading") return t("models.catalog.loading");
  if (c.state === "error") return t("models.catalog.error", { error: c.error });
  const m = c.models;
  const n = (m.models ?? []).length;
  if (m.status === "unsupported" || n === 0) return m.message || t("models.catalog.none");
  if (m.source === "provider") return t("models.catalog.provider", { n, provider: m.provider });
  return m.message || t("models.catalog.builtin");
}
