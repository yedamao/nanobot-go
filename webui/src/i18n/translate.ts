import type { Locale } from "./config.ts";
import { en, type Messages } from "./en.ts";
import { zhCN } from "./zh-CN.ts";

export const catalogs: Record<Locale, Messages> = { en, "zh-CN": zhCN };

type Leaves<T, P extends string = ""> = {
  [K in keyof T & string]: T[K] extends string ? `${P}${K}` : Leaves<T[K], `${P}${K}.`>;
}[keyof T & string];

/** MessageKey is a dotted path to a message, such as "nav.cron". */
export type MessageKey = Leaves<Messages>;

export type Vars = Record<string, string | number>;

function lookup(tree: unknown, key: string): string | undefined {
  let node = tree;
  for (const part of key.split(".")) {
    if (node === null || typeof node !== "object") return undefined;
    node = (node as Record<string, unknown>)[part];
  }
  return typeof node === "string" ? node : undefined;
}

/**
 * translate returns the locale's message for key with {name} placeholders
 * filled from vars, falling back to English and then to the key itself.
 */
export function translate(
  locale: Locale,
  key: MessageKey | (string & {}),
  vars?: Vars,
  sources: Record<string, unknown> = catalogs,
): string {
  const msg = lookup(sources[locale], key) ?? lookup(sources.en, key) ?? key;
  if (!vars) return msg;
  return msg.replace(/\{(\w+)\}/g, (m, name: string) => (name in vars ? String(vars[name]) : m));
}

/** messageKeys lists a catalog's dotted key paths, sorted. */
export function messageKeys(tree: unknown, prefix = ""): string[] {
  if (tree === null || typeof tree !== "object") return [];
  const keys: string[] = [];
  for (const [k, v] of Object.entries(tree)) {
    if (typeof v === "string") keys.push(prefix + k);
    else keys.push(...messageKeys(v, prefix + k + "."));
  }
  return keys.sort();
}

/** Translator is t(): a message for key with its placeholders filled. */
export type Translator = (key: MessageKey, vars?: Vars) => string;

/** Invalid is why a form cannot be used: a message key and the values
 * of its placeholders, translated by the page with t(key, vars). */
export interface Invalid {
  key: MessageKey;
  vars?: Vars;
}

/** enT is the English Translator, the default of helpers that take one. */
export const enT: Translator = (key, vars) => translate("en", key, vars);
