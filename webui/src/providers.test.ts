import assert from "node:assert/strict";
import { test } from "node:test";
import type { Provider } from "./api.ts";
import { checkProvider, diffProvider, keyStatus, providerDraft } from "./providers.ts";

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
  assert.equal(checkProvider(d), "");
  assert.equal(checkProvider({ ...d, apiKey: "***" }), "API key is a redacted placeholder; paste the real key");
  assert.equal(checkProvider({ ...d, apiKey: saved.apiKeyHint }), "API key is a redacted placeholder; paste the real key");
  assert.equal(checkProvider({ ...d, apiKey: "***", clearKey: true }), "");
  assert.equal(checkProvider({ ...d, apiKey: "a b" }), "API key must not contain spaces");
  assert.equal(checkProvider({ ...d, apiBase: "ftp://x" }), "API base must be an http:// or https:// URL");
  assert.equal(checkProvider({ ...d, apiBase: "localhost:1234" }), "API base must be an http:// or https:// URL");
  assert.equal(checkProvider({ ...d, apiBase: "https://u:p@x.test" }), "API base must not hold credentials; set the API key instead");
  assert.equal(checkProvider({ ...d, apiBase: "http://localhost:1234/v1" }), "");
});

test("keyStatus", () => {
  assert.equal(keyStatus(saved), "Set: sk-a••••xyz1");
  assert.equal(keyStatus({ ...saved, hasApiKey: false, apiKeyHint: "", env_overrides: ["apiKey"] }), "Not set (overridden by the environment)");
});
