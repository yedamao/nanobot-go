import assert from "node:assert/strict";
import { test } from "node:test";
import type { Settings } from "./api.ts";
import { diffSettings, duration, pendingText, savedText, whitelistLines, withCurrent } from "./settings.ts";

const saved: Settings = {
  model: "deepseek-chat",
  provider: "auto",
  fallbackModels: [{ provider: "openai", model: "gpt-4o" }],
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
    ssrf_whitelist: ["100.64.0.0/10"],
    web_search: { provider: "", max_results: 5, timeout: 30, key_source: "providers.deepseek.apiKey" },
    web_fetch_provider: "",
  },
  channels: { websocket: { enabled: true }, telegram: { enabled: false } },
};

const clone = (s: Settings): Settings => JSON.parse(JSON.stringify(s));

test("an unchanged form is an empty patch", () => {
  const draft = clone(saved);
  draft.model = "  deepseek-chat ";
  assert.deepEqual(diffSettings(saved, draft), {});
});

test("only changed fields are patched", () => {
  const draft = clone(saved);
  draft.provider = "anthropic";
  draft.tools.exec = false;
  draft.channels.telegram.enabled = true;
  draft.channels.unknown = { enabled: true };
  assert.deepEqual(diffSettings(saved, draft), {
    provider: "anthropic",
    tools: { exec: false },
    channels: { telegram: { enabled: true } },
  });
});

test("image generation fields are patched trimmed and on their own", () => {
  const draft = clone(saved);
  draft.tools.image_generation.enabled = true;
  draft.tools.image_generation.provider = "openai";
  draft.tools.image_generation.model = " gpt-image-1 ";
  draft.tools.image_generation.default_image_size = " 1K ";
  draft.tools.image_generation.max_images_per_turn = 2;
  assert.deepEqual(diffSettings(saved, draft), {
    tools: { image_generation: { enabled: true, provider: "openai", model: "gpt-image-1", max_images_per_turn: 2 } },
  });
});

test("web search and fetch provider are patched without the key source", () => {
  const draft = clone(saved);
  draft.tools.web_search!.key_source = "environment";
  assert.deepEqual(diffSettings(saved, draft), {});
  draft.tools.web_search!.provider = "deepseek";
  draft.tools.web_search!.max_results = 10;
  draft.tools.web_fetch_provider = "deepseek";
  assert.deepEqual(diffSettings(saved, draft), {
    tools: { web_search: { provider: "deepseek", max_results: 10 }, web_fetch_provider: "deepseek" },
  });
  // An older gateway without the fields sends nothing for them.
  const old = clone(saved);
  delete old.tools.web_search;
  delete old.tools.web_fetch_provider;
  assert.deepEqual(diffSettings(old, clone(old)), {});
});

test("the SSRF whitelist is patched as a whole list, trimmed, without blank lines", () => {
  const draft = clone(saved);
  draft.tools.ssrf_whitelist = [" 100.64.0.0/10 ", "", "  "];
  assert.deepEqual(diffSettings(saved, draft), {});
  draft.tools.ssrf_whitelist = ["100.64.0.0/10", " 10.19.3.17", ""];
  assert.deepEqual(diffSettings(saved, draft), { tools: { ssrf_whitelist: ["100.64.0.0/10", "10.19.3.17"] } });
  draft.tools.ssrf_whitelist = [""];
  assert.deepEqual(diffSettings(saved, draft), { tools: { ssrf_whitelist: [] } });
  assert.deepEqual(diffSettings({ ...saved, tools: { ...saved.tools, ssrf_whitelist: null } }, draft), {});
  assert.deepEqual(whitelistLines(null), []);
});

test("savedText names the settings applied at once", () => {
  const view = {
    settings: saved,
    restart_required: [],
    applied: ["tools.ssrf_whitelist"],
    env_overrides: null,
    choices: { provider: null, exec_sandbox: null, image_generation_provider: null },
  };
  assert.equal(savedText(view), "Saved. Applied to the running gateway: tools.ssrf_whitelist.");
  assert.equal(
    savedText({ ...view, applied: null, restart_required: ["tools.exec"] }),
    "Saved — restart the gateway to apply. Restart the gateway to apply: tools.exec.",
  );
});

test("pendingText tolerates null lists", () => {
  const view = {
    settings: saved,
    restart_required: null,
    env_overrides: ["model"],
    choices: { provider: null, exec_sandbox: null, image_generation_provider: null },
  };
  assert.equal(pendingText(view), "Set by an environment variable, which wins over the saved value: model.");
});

test("withCurrent keeps an unknown current value", () => {
  assert.deepEqual(withCurrent(["", "seatbelt"], "bwrap"), ["bwrap", "", "seatbelt"]);
  assert.deepEqual(withCurrent(null, ""), [""]);
});

test("duration", () => {
  assert.equal(duration(9), "9s");
  assert.equal(duration(423), "7m 3s");
  assert.equal(duration(7500), "2h 5m");
  assert.equal(duration(273600), "3d 4h");
});
