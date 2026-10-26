import assert from "node:assert/strict";
import { test } from "node:test";
import type { Settings } from "./api.ts";
import { diffSettings, duration, pendingText, withCurrent } from "./settings.ts";

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
