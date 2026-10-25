import type { FallbackModel, Settings, SettingsPatch } from "./api.ts";

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
