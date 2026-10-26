import assert from "node:assert/strict";
import { test } from "node:test";
import type { Provider } from "./api.ts";
import { canTest, checkProvider, diffProvider, keyStatus, providerDraft, testBody, testNote } from "./providers.ts";

const saved: Provider = {
  name: "openai",
  hasApiKey: true,
  apiKeyHint: "sk-a••••xyz1",
  apiBase: "https://example.test/v1",
  restart_required: false,
  env_overrides: [],
};

test("an untouched draft is an empty patch and never holds the key", () => {
  const d = providerDraft(saved);
  assert.equal(d.apiKey, "");
  assert.deepEqual(diffProvider(saved, d), {});
  assert.deepEqual(diffProvider(saved, { ...d, apiBase: " https://example.test/v1 " }), {});
});

test("diffProvider sets, clears and edits", () => {
  const d = providerDraft(saved);
  assert.deepEqual(diffProvider(saved, { ...d, apiKey: " sk-new " }), { apiKey: "sk-new" });
  assert.deepEqual(diffProvider(saved, { ...d, apiKey: "sk-new", clearKey: true }), { clearApiKey: true });
  assert.deepEqual(diffProvider({ ...saved, hasApiKey: false }, { ...d, clearKey: true }), {});
  assert.deepEqual(diffProvider(saved, { ...d, apiBase: "" }), { apiBase: "" });
});

test("checkProvider", () => {
  const d = providerDraft(saved);
  assert.deepEqual(checkProvider(d), null);
  assert.deepEqual(checkProvider({ ...d, apiKey: "***" }), { key: "providers.validation.keyRedacted" });
  assert.deepEqual(checkProvider({ ...d, apiKey: saved.apiKeyHint }), { key: "providers.validation.keyRedacted" });
  assert.deepEqual(checkProvider({ ...d, apiKey: "***", clearKey: true }), null);
  assert.deepEqual(checkProvider({ ...d, apiKey: "a b" }), { key: "providers.validation.keySpaces" });
  assert.deepEqual(checkProvider({ ...d, apiBase: "ftp://x" }), { key: "providers.validation.baseUrl" });
  assert.deepEqual(checkProvider({ ...d, apiBase: "localhost:1234" }), { key: "providers.validation.baseUrl" });
  assert.deepEqual(checkProvider({ ...d, apiBase: "https://u:p@x.test" }), { key: "providers.validation.baseCredentials" });
  assert.deepEqual(checkProvider({ ...d, apiBase: "http://localhost:1234/v1" }), null);
});

test("keyStatus", () => {
  assert.equal(keyStatus(saved), "Set: sk-a••••xyz1");
  assert.equal(keyStatus({ ...saved, hasApiKey: false, apiKeyHint: "", env_overrides: ["apiKey"] }), "Not set (overridden by the environment)");
});

test("testBody sends only the drafts", () => {
  const d = providerDraft(saved);
  assert.deepEqual(testBody(saved, d), {});
  assert.deepEqual(testBody(saved, { ...d, apiKey: " sk-draft " }), { apiKey: "sk-draft" });
  assert.deepEqual(testBody(saved, { ...d, apiKey: "sk-draft", clearKey: true }), {});
  assert.deepEqual(testBody(saved, { ...d, apiBase: " http://localhost:1/v1 " }), { apiBase: "http://localhost:1/v1" });
  assert.deepEqual(testBody(saved, { ...d, apiBase: "" }), { apiBase: "" });
});

test("canTest needs a key", () => {
  const none: Provider = { ...saved, hasApiKey: false, apiKeyHint: "" };
  assert.equal(canTest(saved, providerDraft(saved)), true);
  assert.equal(canTest(saved, { ...providerDraft(saved), clearKey: true }), false);
  assert.equal(canTest(none, providerDraft(none)), false);
  assert.equal(canTest(none, { ...providerDraft(none), apiKey: " " }), false);
  assert.equal(canTest(none, { ...providerDraft(none), apiKey: "sk-x" }), true);
  assert.equal(canTest({ ...none, env_overrides: ["apiKey"] }, providerDraft(none)), true);
});

test("testNote", () => {
  assert.deepEqual(testNote({ ok: true, status: "ok", message: "openai accepted the credential." }), {
    kind: "ok",
    text: "Test passed: openai accepted the credential.",
  });
  assert.deepEqual(testNote({ ok: false, status: "rejected", message: "" }), { kind: "error", text: "Test failed: The test failed." });
});
