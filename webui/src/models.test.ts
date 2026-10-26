import assert from "node:assert/strict";
import { test } from "node:test";
import type { ProviderModels, Settings } from "./api.ts";
import { catalogListId, catalogNote, catalogOptions, checkModels, diffModels, draftProviders, maxFallbacks, modelsDraft, moveItem } from "./models.ts";

const saved: Settings = {
  model: "claude-x",
  provider: "anthropic",
  fallbackModels: [
    { provider: "openai", model: "gpt-4o" },
    { provider: "auto", model: "deepseek-chat" },
  ],
  tools: {
    exec: true,
    web_fetch: true,
    exec_sandbox: "",
    image_generation: {
      enabled: false,
      provider: "openrouter",
      model: "openai/gpt-5.4-image-2",
      default_aspect_ratio: "1:1",
      default_image_size: "1K",
      max_images_per_turn: 4,
    },
  },
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
  assert.deepEqual(checkModels(d), null);
  assert.deepEqual(checkModels({ ...d, model: " " }), { key: "models.validation.modelEmpty" });
  assert.deepEqual(checkModels({ ...d, fallbacks: [{ provider: "auto", model: "a b" }] }), { key: "models.validation.fallbackSpaces", vars: { n: 1 } });
  assert.deepEqual(checkModels({ ...d, fallbacks: Array(maxFallbacks + 1).fill({ provider: "auto", model: "m" }) }), { key: "models.validation.tooManyFallbacks", vars: { max: 16 } });
  assert.deepEqual(checkModels({ ...d, fallbacks: [{ provider: "auto", model: "ok" }, { provider: "auto", model: "x".repeat(201) }] }), {
    key: "models.validation.fallbackTooLong",
    vars: { n: 2, max: 200 },
  });
});

test("moveItem", () => {
  assert.deepEqual(moveItem([1, 2, 3], 0, 1), [2, 1, 3]);
  assert.deepEqual(moveItem([1, 2, 3], 2, -1), [1, 3, 2]);
  const l = [1, 2];
  assert.equal(moveItem(l, 0, -1), l);
  assert.equal(moveItem(l, 1, 1), l);
});

test("draftProviders lists each row's provider once", () => {
  const d = modelsDraft(saved);
  assert.deepEqual(draftProviders(d), ["anthropic", "openai", "auto"]);
  assert.deepEqual(draftProviders({ ...d, fallbacks: [{ provider: "", model: "m" }, { provider: "anthropic", model: "n" }] }), ["anthropic", "auto"]);
});

test("catalog suggestions and notes", () => {
  const curated: ProviderModels = {
    provider: "anthropic",
    catalog_kind: "curated",
    status: "available",
    message: "Built-in list.",
    source: "curated",
    models: [{ id: "claude-a" }, { id: "gpt-x", label: "openai" }],
  };
  assert.deepEqual(catalogOptions({ state: "ok", models: curated }), [
    { value: "claude-a", label: "" },
    { value: "gpt-x", label: "openai" },
  ]);
  assert.deepEqual(catalogOptions({ state: "loading" }), []);
  assert.deepEqual(catalogOptions(undefined), []);
  assert.equal(catalogNote(undefined), "");
  assert.equal(catalogNote({ state: "ok", models: curated }), "Built-in list.");
  assert.equal(
    catalogNote({ state: "ok", models: { ...curated, provider: "openai", catalog_kind: "fetched", source: "provider", message: undefined } }),
    "2 models listed by openai. Any model ID can be typed.",
  );
  assert.equal(
    catalogNote({ state: "ok", models: { ...curated, status: "not_configured", message: "No API key." } }),
    "No API key.",
  );
  assert.equal(catalogNote({ state: "error", error: "HTTP 500" }), "No model suggestions (HTTP 500). Type a model ID.");
  assert.equal(catalogNote({ state: "ok", models: { ...curated, models: null, message: undefined } }), "No model list for this provider. Type a model ID.");
  assert.equal(catalogListId("open/router x"), "models-catalog-open_router_x");
  assert.equal(catalogListId(""), "models-catalog-auto");
});
