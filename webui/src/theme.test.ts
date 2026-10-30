import assert from "node:assert/strict";
import { test } from "node:test";
import {
  THEME_STORAGE_KEY,
  applyTheme,
  defaultThemePreference,
  initialThemePreference,
  normalizeThemePreference,
  persistThemePreference,
  resolveTheme,
  themePreferences,
} from "./themePref.ts";

class MemStore {
  data = new Map<string, string>();
  getItem(key: string) {
    return this.data.has(key) ? this.data.get(key)! : null;
  }
  setItem(key: string, value: string) {
    this.data.set(key, value);
  }
}

test("theme preferences are system, light, dark; default is system", () => {
  assert.deepEqual([...themePreferences], ["system", "light", "dark"]);
  assert.equal(defaultThemePreference, "system");
  assert.equal(THEME_STORAGE_KEY, "nanobot.theme");
});

test("normalizeThemePreference accepts known values and falls back to system", () => {
  for (const p of ["system", "light", "dark", " Light ", "DARK"]) {
    assert.equal(normalizeThemePreference(p), p.trim().toLowerCase());
  }
  for (const bad of ["", "auto", "bogus", null, undefined]) {
    assert.equal(normalizeThemePreference(bad as string | null | undefined), "system");
  }
});

test("resolveTheme maps preference onto a palette", () => {
  assert.equal(resolveTheme("light", true), "light");
  assert.equal(resolveTheme("light", false), "light");
  assert.equal(resolveTheme("dark", true), "dark");
  assert.equal(resolveTheme("dark", false), "dark");
  assert.equal(resolveTheme("system", true), "dark");
  assert.equal(resolveTheme("system", false), "light");
});

test("persistThemePreference writes nanobot.theme and initialThemePreference reads it", () => {
  const store = new MemStore();
  persistThemePreference("light", store);
  assert.equal(store.data.get("nanobot.theme"), "light");
  assert.equal(initialThemePreference(store), "light");
  persistThemePreference("dark", store);
  assert.equal(initialThemePreference(store), "dark");
  persistThemePreference("system", store);
  assert.equal(initialThemePreference(store), "system");
});

test("storage errors do not break theme handling", () => {
  const broken = {
    getItem() {
      throw new Error("denied");
    },
    setItem() {
      throw new Error("denied");
    },
  };
  assert.doesNotThrow(() => persistThemePreference("light", broken));
  assert.equal(initialThemePreference(broken), "system");
});

test("applyTheme sets data-theme and color-scheme on the document element", () => {
  const el = { dataset: {} as Record<string, string>, style: { colorScheme: "" } };
  const doc = { documentElement: el } as unknown as Document;
  applyTheme("light", doc);
  assert.equal(el.dataset.theme, "light");
  assert.equal(el.style.colorScheme, "light");
  applyTheme("dark", doc);
  assert.equal(el.dataset.theme, "dark");
  assert.equal(el.style.colorScheme, "dark");
});
