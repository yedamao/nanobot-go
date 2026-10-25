import assert from "node:assert/strict";
import { test } from "node:test";
import type { Settings } from "./api.ts";
import { diffSettings, duration, pendingText, withCurrent } from "./settings.ts";

const saved: Settings = {
  model: "deepseek-chat",
  provider: "auto",
  tools: { exec: true, web_fetch: true, exec_sandbox: "" },
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

test("pendingText tolerates null lists", () => {
  const view = { settings: saved, restart_required: null, env_overrides: ["model"], choices: { provider: null, exec_sandbox: null } };
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
