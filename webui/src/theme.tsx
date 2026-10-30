import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { useT } from "./i18n";
import {
  applyTheme,
  bootTheme,
  persistThemePreference,
  resolveTheme,
  themePreferences,
  type ThemePreference,
} from "./themePref";

interface ThemeState {
  preference: ThemePreference;
  setPreference: (p: ThemePreference) => void;
}

const ThemeContext = createContext<ThemeState>({
  preference: "system",
  setPreference: () => {},
});

// ThemeProvider holds System | Light | Dark. System tracks the OS scheme
// live; the choice is persisted and applied as data-theme on <html>.
export function ThemeProvider({ children }: { children: ReactNode }) {
  const [preference, setPreferenceState] = useState<ThemePreference>(() => bootTheme());

  useEffect(() => {
    applyTheme(resolveTheme(preference));
    if (preference !== "system") return;
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => applyTheme(resolveTheme("system", mq.matches));
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [preference]);

  const setPreference = useCallback((p: ThemePreference) => {
    persistThemePreference(p);
    setPreferenceState(p);
  }, []);

  return <ThemeContext.Provider value={{ preference, setPreference }}>{children}</ThemeContext.Provider>;
}

export function useTheme(): ThemeState {
  return useContext(ThemeContext);
}

const LABELS: Record<ThemePreference, "app.theme.system" | "app.theme.light" | "app.theme.dark"> = {
  system: "app.theme.system",
  light: "app.theme.light",
  dark: "app.theme.dark",
};

// ThemeSwitcher is the Appearance control: System / Light / Dark.
export function ThemeSwitcher({ id }: { id?: string }) {
  const { preference, setPreference } = useTheme();
  const t = useT();
  return (
    <div id={id} className="seg theme-seg" role="group" aria-label={t("app.appearance")}>
      {themePreferences.map((p) => (
        <button
          key={p}
          type="button"
          title={t(LABELS[p])}
          aria-pressed={preference === p}
          onClick={() => setPreference(p)}
        >
          {t(LABELS[p])}
        </button>
      ))}
    </div>
  );
}

export type { ThemePreference, ResolvedTheme } from "./themePref";
export {
  THEME_STORAGE_KEY,
  themePreferences,
  defaultThemePreference,
  normalizeThemePreference,
  resolveTheme,
  applyTheme,
  initialThemePreference,
  persistThemePreference,
  bootTheme,
} from "./themePref";
