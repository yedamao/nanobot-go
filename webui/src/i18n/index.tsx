import { Fragment, createContext, useCallback, useContext, useEffect, useState, type ReactNode } from "react";
import { initialLocale, normalizeLocale, persistLocale, supportedLocales, type Locale } from "./config.ts";
import { translate, type Translator } from "./translate.ts";

export type { Locale } from "./config.ts";
export type { MessageKey, Translator } from "./translate.ts";

export type T = Translator;

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

// LocaleSwitcher picks the UI language: a segmented EN / 中文 control,
// each option titled with its language's own name.
export function LocaleSwitcher({ id }: { id?: string }) {
  const { locale, setLocale } = useLocale();
  const t = useT();
  return (
    <div id={id} className="seg" role="group" aria-label={t("app.language")}>
      {supportedLocales.map((l) => (
        <button
          key={l.code}
          type="button"
          lang={l.code}
          title={l.nativeLabel}
          aria-pressed={locale === l.code}
          onClick={() => setLocale(normalizeLocale(l.code))}
        >
          {l.shortLabel}
        </button>
      ))}
    </div>
  );
}

/** useFormatDate formats an ISO time (or Date) in the selected locale;
 * an unparsable string is returned as is. */
export function useFormatDate(): (iso?: string | Date) => string {
  const { locale } = useLocale();
  return useCallback(
    (iso) => {
      if (!iso) return "";
      const d = iso instanceof Date ? iso : new Date(iso);
      return Number.isNaN(d.getTime()) ? String(iso) : d.toLocaleString(locale);
    },
    [locale],
  );
}

/**
 * Fill renders a translated message whose {name} placeholders are
 * elements, such as <code> or a link: Fill text={t("x")} vars={{ path: <code>p</code> }}.
 */
export function Fill({ text, vars }: { text: string; vars: Record<string, ReactNode> }) {
  const parts = text.split(/\{(\w+)\}/);
  return (
    <>
      {parts.map((part, i) =>
        i % 2 === 0 ? part : <Fragment key={i}>{part in vars ? vars[part] : "{" + part + "}"}</Fragment>,
      )}
    </>
  );
}
