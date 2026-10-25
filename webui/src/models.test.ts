import assert from "node:assert/strict";
import { test } from "node:test";
import type { Settings } from "./api.ts";
import { checkModels, diffModels, maxFallbacks, modelsDraft, moveItem } from "./models.ts";

const saved: Settings = {
  model: "claude-x",
  provider: "anthropic",
  fallbackModels: [
    { provider: "openai", model: "gpt-4o" },
    { provider: "auto", model: "deepseek-chat" },
  ],
  tools: { exec: true, web_fetch: true, exec_sandbox: "" },
  channels: {},
};

test("an unchanged draft is an empty patch", () => {
  const d = modelsDraft(saved);
  d.model = " claude-x ";
  assert.deepEqual(diffModels(saved, d), {});
  assert.deepEqual(modelsDraft({ ...saved, fallbackModels: null }).fallbacks, []);
});

test("fallbacks are sent whole on any change, trimmed", () => {
  const d = modelsDraft(saved);
  d.fallbacks = moveItem(d.fallbacks, 1, -1);
  assert.deepEqual(diffModels(saved, d), { fallbackModels: [saved.fallbackModels![1], saved.fallbackModels![0]] });
  const e = modelsDraft(saved);
  e.fallbacks[0].model = " gpt-5 ";
  e.provider = "openai";
  assert.deepEqual(diffModels(saved, e), {
    provider: "openai",
    fallbackModels: [{ provider: "openai", model: "gpt-5" }, saved.fallbackModels![1]],
  });
  assert.deepEqual(diffModels(saved, { ...modelsDraft(saved), fallbacks: [] }), { fallbackModels: [] });
  assert.deepEqual(diffModels({ ...saved, fallbackModels: [] }, { ...modelsDraft(saved), fallbacks: [{ provider: "", model: "m" }] }), {
    fallbackModels: [{ provider: "auto", model: "m" }],
  });
});

test("checkModels", () => {
  const d = modelsDraft(saved);
  assert.equal(checkModels(d), "");
  assert.equal(checkModels({ ...d, model: " " }), "Model must not be empty");
  assert.equal(checkModels({ ...d, fallbacks: [{ provider: "auto", model: "a b" }] }), "Fallback 1 model must not contain spaces");
  assert.equal(checkModels({ ...d, fallbacks: Array(maxFallbacks + 1).fill({ provider: "auto", model: "m" }) }), "at most 16 fallbacks");
});

test("moveItem", () => {
  assert.deepEqual(moveItem([1, 2, 3], 0, 1), [2, 1, 3]);
  assert.deepEqual(moveItem([1, 2, 3], 2, -1), [1, 3, 2]);
  const l = [1, 2];
  assert.equal(moveItem(l, 0, -1), l);
  assert.equal(moveItem(l, 1, 1), l);
});
