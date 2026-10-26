import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { initialLocale, normalizeLocale, persistLocale, supportedLocales, type Locale } from "./config.ts";
import { translate, type MessageKey, type Vars } from "./translate.ts";

export type { Locale } from "./config.ts";
export type { MessageKey } from "./translate.ts";

export type T = (key: MessageKey, vars?: Vars) => string;

interface LocaleState {
  locale: Locale;
  setLocale: (l: Locale) => void;
}

const LocaleContext = createContext<LocaleState>({ locale: "en", setLocale: () => {} });

// LocaleProvider holds the UI language: the stored choice, else the
// browser's, else English. Changing it re-renders at once and persists.
export function LocaleProvider({ children }: { children: ReactNode }) {
  const [locale, setLocaleState] = useState<Locale>(() => initialLocale());

  useEffect(() => {
    document.documentElement.lang = locale;
  }, [locale]);

  const setLocale = useCallback((l: Locale) => {
    persistLocale(l);
    setLocaleState(l);
  }, []);

  return <LocaleContext.Provider value={{ locale, setLocale }}>{children}</LocaleContext.Provider>;
}

export function useLocale(): LocaleState {
  return useContext(LocaleContext);
}

/** useT returns t("nav.cron") for the current locale. */
export function useT(): T {
  const { locale } = useLocale();
  return useCallback<T>((key, vars) => translate(locale, key, vars), [locale]);
}

// LocaleSwitcher picks the UI language, each named in its own language.
export function LocaleSwitcher({ id }: { id?: string }) {
  const { locale, setLocale } = useLocale();
  const t = useT();
  return (
    <select
      id={id}
      className="locale-switcher"
      aria-label={t("app.language")}
      title={t("app.language")}
      value={locale}
      onChange={(e) => setLocale(normalizeLocale(e.target.value))}
    >
      {supportedLocales.map((l) => (
        <option key={l.code} value={l.code} lang={l.code}>
          {l.nativeLabel}
        </option>
      ))}
    </select>
  );
}
