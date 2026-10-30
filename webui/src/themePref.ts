// Theme preference: system (default) follows prefers-color-scheme;
// light / dark force a palette. Stored in localStorage like the locale.
export const THEME_STORAGE_KEY = "nanobot.theme";

export const themePreferences = ["system", "light", "dark"] as const;
export type ThemePreference = (typeof themePreferences)[number];
export type ResolvedTheme = "light" | "dark";

export const defaultThemePreference: ThemePreference = "system";

export function isThemePreference(v: string | null | undefined): v is ThemePreference {
  return v === "system" || v === "light" || v === "dark";
}

/** normalizeThemePreference keeps a known preference, else system. */
export function normalizeThemePreference(input: string | null | undefined): ThemePreference {
  const t = (input ?? "").trim().toLowerCase();
  return isThemePreference(t) ? t : defaultThemePreference;
}

/** systemPrefersDark reads the OS color scheme (or a test MediaQueryList). */
export function systemPrefersDark(mq?: { matches: boolean } | null): boolean {
  if (mq) return mq.matches;
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return true;
  try {
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  } catch {
    return true;
  }
}

/** resolveTheme maps a preference onto the palette to paint. */
export function resolveTheme(preference: ThemePreference, systemDark: boolean = systemPrefersDark()): ResolvedTheme {
  if (preference === "light") return "light";
  if (preference === "dark") return "dark";
  return systemDark ? "dark" : "light";
}

/** applyTheme sets data-theme and color-scheme on <html>. */
export function applyTheme(resolved: ResolvedTheme, doc: Document | null = typeof document !== "undefined" ? document : null): void {
  if (!doc) return;
  const root = doc.documentElement;
  root.dataset.theme = resolved;
  root.style.colorScheme = resolved;
}

interface KeyValueStore {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

function storage(): KeyValueStore | null {
  try {
    return typeof localStorage === "undefined" ? null : localStorage;
  } catch {
    return null;
  }
}

/** initialThemePreference reads localStorage; missing or bad → system. */
export function initialThemePreference(store: KeyValueStore | null = storage()): ThemePreference {
  try {
    return normalizeThemePreference(store?.getItem(THEME_STORAGE_KEY));
  } catch {
    return defaultThemePreference;
  }
}

/** persistThemePreference stores the choice (not the resolved palette). */
export function persistThemePreference(preference: ThemePreference, store: KeyValueStore | null = storage()): void {
  try {
    store?.setItem(THEME_STORAGE_KEY, preference);
  } catch {
    // ignore storage errors
  }
}

/** bootTheme applies the stored preference before React paints (cuts FOUC). */
export function bootTheme(): ThemePreference {
  const preference = initialThemePreference();
  applyTheme(resolveTheme(preference));
  return preference;
}
