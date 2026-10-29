import assert from "node:assert/strict";
import { test } from "node:test";
import {
  LOCALE_STORAGE_KEY,
  initialLocale,
  matchLocale,
  normalizeLocale,
  persistLocale,
  resolveInitialLocale,
  supportedLocales,
} from "./i18n/config.ts";
import { en } from "./i18n/en.ts";
import { catalogs, messageKeys, translate } from "./i18n/translate.ts";
import { zhCN } from "./i18n/zh-CN.ts";

function memoryStore(init: Record<string, string> = {}) {
  const data = new Map(Object.entries(init));
  return {
    data,
    getItem: (k: string) => data.get(k) ?? null,
    setItem: (k: string, v: string) => void data.set(k, v),
  };
}

test("supported locales are en and zh-CN with native labels", () => {
  assert.deepEqual(
    supportedLocales.map((l) => [l.code, l.nativeLabel]),
    [
      ["en", "English"],
      ["zh-CN", "简体中文"],
    ],
  );
  assert.equal(LOCALE_STORAGE_KEY, "nanobot.locale");
});

test("normalizeLocale maps Chinese variants to zh-CN and the rest to en", () => {
  for (const tag of ["zh-CN", "zh", "zh-cn", "ZH-CN", " zh-CN ", "zh-Hans", "zh-Hans-CN", "zh-SG"]) {
    assert.equal(normalizeLocale(tag), "zh-CN", tag);
  }
  for (const tag of ["en", "en-US", "en-GB", "fr", "ja", "zh-TW", "", null, undefined]) {
    assert.equal(normalizeLocale(tag), "en", String(tag));
  }
  assert.equal(matchLocale("fr"), null);
  assert.equal(matchLocale("en-AU"), "en");
});

test("resolveInitialLocale prefers storage, then a supported browser language, then en", () => {
  assert.equal(resolveInitialLocale("en", ["zh-CN"]), "en");
  assert.equal(resolveInitialLocale("zh-CN", ["en-US"]), "zh-CN");
  assert.equal(resolveInitialLocale(null, ["zh-CN", "en"]), "zh-CN");
  assert.equal(resolveInitialLocale(null, ["fr-FR", "zh"]), "zh-CN");
  assert.equal(resolveInitialLocale("bogus", ["fr", "de"]), "en");
  assert.equal(resolveInitialLocale(null, []), "en");
});

test("persistLocale writes nanobot.locale and initialLocale reads it back", () => {
  const store = memoryStore();
  persistLocale("zh-CN", store);
  assert.equal(store.data.get("nanobot.locale"), "zh-CN");
  assert.equal(initialLocale(store), "zh-CN");
  persistLocale("en", store);
  assert.equal(initialLocale(store), "en");
});

test("storage errors do not break locale handling", () => {
  const broken = {
    getItem: (): string | null => {
      throw new Error("denied");
    },
    setItem: () => {
      throw new Error("denied");
    },
  };
  assert.doesNotThrow(() => persistLocale("zh-CN", broken));
  assert.ok(["en", "zh-CN"].includes(initialLocale(broken)));
});

test("translate looks up dotted keys and fills placeholders", () => {
  assert.equal(translate("en", "nav.cron"), "Cron");
  assert.equal(translate("zh-CN", "nav.cron"), "定时任务");
  assert.equal(translate("en", "connection.closed", { seconds: 3 }), "Offline · retrying in 3s");
  assert.equal(translate("zh-CN", "cron.history.title", { name: "daily" }), "运行历史：daily");
  assert.equal(translate("en", "cron.notes.ran", {}), "Ran {name}", "unknown placeholders stay");
});

test("translate falls back from zh-CN to en, and from en to the key", () => {
  const sources = { en: { a: { b: "B", c: "C" } }, "zh-CN": { a: { b: "乙" } } };
  assert.equal(translate("zh-CN", "a.b", undefined, sources), "乙");
  assert.equal(translate("zh-CN", "a.c", undefined, sources), "C");
  assert.equal(translate("zh-CN", "a.missing", undefined, sources), "a.missing");
  assert.equal(translate("en", "a.missing", undefined, sources), "a.missing");
  assert.equal(translate("en", "a", undefined, sources), "a", "a branch is not a message");
  assert.equal(translate("en", "nope.nothing"), "nope.nothing");
});

test("zh-CN has exactly the keys of en, all non-empty", () => {
  assert.deepEqual(messageKeys(zhCN), messageKeys(en));
  for (const locale of Object.keys(catalogs) as (keyof typeof catalogs)[]) {
    for (const key of messageKeys(catalogs[locale])) {
      assert.ok(translate(locale, key).trim(), locale + " " + key);
    }
  }
});

test("zh-CN keeps every placeholder of the English message", () => {
  const names = (s: string) => [...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();
  for (const key of messageKeys(en)) {
    assert.deepEqual(names(translate("zh-CN", key)), names(translate("en", key)), key);
  }
});

test("display helpers localize with a zh-CN translator and default to English", async () => {
  const { keyStatus, testNote } = await import("./providers.ts");
  const { pendingText } = await import("./settings.ts");
  const { mcpBadges } = await import("./mcp.ts");
  const zh = (key: Parameters<typeof translate>[1], vars?: Record<string, string | number>) => translate("zh-CN", key, vars);
  const p = { name: "openai", hasApiKey: true, apiKeyHint: "sk-…1", env_overrides: ["apiKey"] } as unknown as Parameters<typeof keyStatus>[0];
  assert.equal(keyStatus(p), "Set: sk-…1 (overridden by the environment)");
  assert.equal(keyStatus(p, zh), "已设置：sk-…1（被环境变量覆盖）");
  assert.deepEqual(testNote({ ok: false, status: "rejected", message: "" }, zh), { kind: "error", text: "测试失败：测试未通过。" });
  const view = { restart_required: ["model"], env_overrides: [] } as unknown as Parameters<typeof pendingText>[0];
  assert.equal(pendingText(view, zh), "重启网关以应用：model。");
  const s = { transport: "stdio", enabled: false, restart_required: true } as unknown as Parameters<typeof mcpBadges>[0];
  assert.deepEqual(mcpBadges(s, zh), ["stdio", "已停用", "需要重启"]);
});

test("validation keys are translated, fully Chinese in zh-CN", async () => {
  const { checkToken } = await import("./channelsConfig.ts");
  const { checkModels } = await import("./models.ts");
  const { checkProvider } = await import("./providers.ts");
  const { buildMCPInput, emptyMCPForm } = await import("./mcp.ts");
  const keys = messageKeys(en).filter((k) => /^(channels|models|providers|mcp)\.validation\./.test(k));
  assert.ok(keys.length >= 25, String(keys.length));
  for (const key of keys) {
    const zh = translate("zh-CN", key);
    assert.match(zh, /\p{Script=Han}/u, key);
    assert.doesNotMatch(zh, /\bmust\b|\bneeds?\b/, key);
  }
  const bad = checkToken("telegram", "a b")!;
  assert.equal(translate("zh-CN", "common.notSavedInvalid", { message: translate("zh-CN", bad.key, bad.vars) }), "未保存：令牌不能包含空格。");
  const m = checkModels({ model: "m", provider: "auto", fallbacks: [{ provider: "auto", model: "" }] })!;
  assert.equal(translate("zh-CN", m.key, m.vars), "备用模型 1 不能为空");
  const p = checkProvider({ apiKey: "", clearKey: false, apiBase: "ftp://x" })!;
  assert.equal(translate("en", p.key, p.vars), "the API base must be an http:// or https:// URL");
  const built = buildMCPInput({ ...emptyMCPForm, name: "x", command: "c", env: "NOPE" });
  assert.ok("error" in built);
  assert.equal(translate("zh-CN", built.error.key, built.error.vars), "环境变量行“NOPE…”应为 KEY=VALUE 格式");
});
