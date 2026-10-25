import assert from "node:assert/strict";
import { test } from "node:test";
import type { ChannelConfig } from "./api.ts";
import {
  canTest,
  channelDraft,
  channelState,
  checkChannel,
  checkToken,
  diffChannel,
  parseAllowFrom,
  testNote,
  tokenStatus,
} from "./channelsConfig.ts";

const telegram: ChannelConfig = {
  name: "telegram",
  present: true,
  enabled: true,
  hasToken: true,
  tokenHint: "123456:••••abcd",
  allowFrom: ["*"],
  pollTimeout: 0,
  apiBase: "",
  groupPolicy: "mention",
  defaultBase: "https://api.telegram.org",
  env_overrides: [],
  restart_required: false,
  running: true,
  testSupported: true,
};

const weixin: ChannelConfig = {
  name: "weixin",
  present: false,
  enabled: false,
  hasToken: false,
  tokenHint: "",
  allowFrom: [],
  pollTimeout: 0,
  hasLoginState: true,
  defaultBase: "https://ilinkai.weixin.qq.com",
  env_overrides: [],
  restart_required: false,
  running: false,
  testSupported: false,
};

test("an untouched draft is an empty patch and never holds the token", () => {
  const d = channelDraft(telegram);
  assert.equal(d.token, "");
  assert.deepEqual(diffChannel(telegram, d), {});
  assert.deepEqual(diffChannel(telegram, { ...d, allowFrom: " * , " }), {});
});

test("a missing section is created with its enabled flag", () => {
  const d = channelDraft(weixin);
  assert.deepEqual(diffChannel(weixin, d), { enabled: false });
  assert.deepEqual(diffChannel(weixin, { ...d, enabled: true, baseUrl: " https://x.test ", routeTag: "7" }), {
    enabled: true,
    baseUrl: "https://x.test",
    routeTag: "7",
  });
});

test("diffChannel sets, clears and edits", () => {
  const d = channelDraft(telegram);
  assert.deepEqual(diffChannel(telegram, { ...d, token: " 1:new " }), { token: "1:new" });
  assert.deepEqual(diffChannel(telegram, { ...d, token: "1:new", clearToken: true }), { clearToken: true });
  assert.deepEqual(diffChannel({ ...telegram, hasToken: false }, { ...d, clearToken: true }), {});
  assert.deepEqual(diffChannel(telegram, { ...d, allowFrom: "123, @alice 123" }), { allowFrom: ["123", "@alice"] });
  assert.deepEqual(diffChannel(telegram, { ...d, allowFrom: "" }), { allowFrom: [] });
  assert.deepEqual(diffChannel(telegram, { ...d, groupPolicy: "open", replyToMessage: true, pollTimeout: "20" }), {
    groupPolicy: "open",
    replyToMessage: true,
    pollTimeout: 20,
  });
  assert.deepEqual(diffChannel(telegram, { ...d, enabled: false, baseUrl: "https://ignored.test" }), { enabled: false });
});

test("parseAllowFrom", () => {
  assert.deepEqual(parseAllowFrom(""), []);
  assert.deepEqual(parseAllowFrom("a,b , a\nc"), ["a", "b", "c"]);
});

test("checkToken", () => {
  assert.equal(checkToken("telegram", ""), "");
  assert.equal(checkToken("telegram", "123456:AA_b-c"), "");
  assert.equal(checkToken("telegram", "***"), "token is a redacted placeholder; paste the real token");
  assert.equal(checkToken("telegram", telegram.tokenHint), "token is a redacted placeholder; paste the real token");
  assert.equal(checkToken("telegram", "a b"), "token must not contain spaces");
  assert.equal(checkToken("telegram", "nocolon"), "token must look like 123456:ABC… as issued by @BotFather");
  assert.equal(checkToken("weixin", "nocolon"), "");
});

test("checkChannel", () => {
  const d = channelDraft(telegram);
  assert.equal(checkChannel(telegram, d), "");
  assert.equal(checkChannel(telegram, { ...d, clearToken: true }), "Telegram needs a bot token to be enabled");
  assert.equal(checkChannel(telegram, { ...d, clearToken: true, enabled: false }), "");
  assert.equal(checkChannel({ ...telegram, env_overrides: ["token"] }, { ...d, clearToken: true }), "");
  assert.equal(checkChannel(telegram, { ...d, apiBase: "ftp://x" }), "API base must be an http:// or https:// URL");
  assert.equal(checkChannel(telegram, { ...d, pollTimeout: "-1" }), "poll timeout must be a whole number of seconds up to 3600");
  assert.equal(checkChannel(weixin, { ...channelDraft(weixin), enabled: true }), "");
  assert.equal(checkChannel(weixin, { ...channelDraft(weixin), baseUrl: "https://u:p@x.test" }), "base URL must not hold credentials");
});

test("tokenStatus and channelState", () => {
  assert.equal(tokenStatus(telegram), "Set: 123456:••••abcd");
  assert.equal(tokenStatus({ ...telegram, hasToken: false, tokenHint: "", env_overrides: ["token"] }), "Not set (overridden by the environment)");
  assert.equal(tokenStatus(weixin), "From 'nanobot channels login weixin'");
  assert.equal(tokenStatus({ ...weixin, hasLoginState: false }), "Not set");
  assert.equal(channelState(telegram), "running");
  assert.equal(channelState({ ...telegram, running: false }), "enabled, not running");
  assert.equal(channelState(weixin), "not in config.json");
});

test("canTest and testNote", () => {
  const d = channelDraft(telegram);
  assert.equal(canTest(telegram, d), true);
  assert.equal(canTest(telegram, { ...d, clearToken: true }), false);
  assert.equal(canTest({ ...telegram, hasToken: false }, d), false);
  assert.equal(canTest({ ...telegram, hasToken: false }, { ...d, token: "1:x" }), true);
  assert.equal(canTest(weixin, channelDraft(weixin)), false);
  assert.deepEqual(testNote({ ok: true, status: "ok", message: "Telegram accepted the token of @bot." }), {
    kind: "ok",
    text: "Test passed: Telegram accepted the token of @bot.",
  });
  assert.deepEqual(testNote({ ok: false, status: "rejected", message: "" }), { kind: "error", text: "Test failed: The test failed." });
  assert.deepEqual(testNote({ ok: false, status: "unsupported", message: "Use the CLI." }), { kind: "", text: "Use the CLI." });
});
