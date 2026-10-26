// Locale settings shared with the Python nanobot WebUI: the same locale
// codes and the same localStorage key, so a browser keeps its choice.
export const LOCALE_STORAGE_KEY = "nanobot.locale";

export const supportedLocales = [
  { code: "en", nativeLabel: "English" },
  { code: "zh-CN", nativeLabel: "简体中文" },
] as const;

export type Locale = (typeof supportedLocales)[number]["code"];

export const defaultLocale: Locale = "en";

function isSupported(code: string): code is Locale {
  return supportedLocales.some((l) => l.code === code);
}

/** matchLocale maps a locale tag onto a supported locale, or null. */
export function matchLocale(input: string | null | undefined): Locale | null {
  const trimmed = (input ?? "").trim();
  if (isSupported(trimmed)) return trimmed;
  const lower = trimmed.toLowerCase();
  if (lower === "zh" || lower.startsWith("zh-cn") || lower.startsWith("zh-sg") || lower.startsWith("zh-hans")) {
    return "zh-CN";
  }
  if (lower === "en" || lower.startsWith("en-")) return "en";
  return null;
}

/** normalizeLocale is matchLocale with English for anything unsupported. */
export function normalizeLocale(input: string | null | undefined): Locale {
  return matchLocale(input) ?? defaultLocale;
}

/**
 * resolveInitialLocale picks the stored choice, else the browser's
 * language when it is supported, else English.
 */
export function resolveInitialLocale(stored: string | null | undefined, browser: readonly (string | null | undefined)[]): Locale {
  for (const tag of [stored, ...browser]) {
    const l = matchLocale(tag);
    if (l) return l;
  }
  return defaultLocale;
}

interface KeyValueStore {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

function storage(): KeyValueStore | null {
  try {
    return typeof localStorage === "undefined" ? null : localStorage;
  } catch {
    return null; // storage disabled
  }
}

/** initialLocale reads localStorage and navigator.languages. */
export function initialLocale(store: KeyValueStore | null = storage()): Locale {
  let stored: string | null = null;
  try {
    stored = store?.getItem(LOCALE_STORAGE_KEY) ?? null;
  } catch {
    // ignore storage errors
  }
  const nav = typeof navigator === "undefined" ? [] : [...(navigator.languages ?? []), navigator.language];
  return resolveInitialLocale(stored, nav);
}

/** persistLocale stores the choice and sets <html lang>. */
export function persistLocale(locale: Locale, store: KeyValueStore | null = storage()): void {
  try {
    store?.setItem(LOCALE_STORAGE_KEY, locale);
  } catch {
    // ignore storage errors
  }
  if (typeof document !== "undefined") document.documentElement.lang = locale;
}
