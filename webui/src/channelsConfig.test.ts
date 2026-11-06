import assert from "node:assert/strict";
import { test } from "node:test";
import type { ChannelConfig, WeixinLogin } from "./api.ts";
import {
  canClearLogin,
  canTest,
  clearLoginPrompt,
  channelDraft,
  channelState,
  checkChannel,
  checkToken,
  diffChannel,
  forceLoginCommand,
  loginDetails,
  loginStatus,
  parseWeixinInstanceInput,
  parseAllowFrom,
  partitionChannelConfigs,
  testNote,
  tokenStatus,
  weixinLogin,
  withLogin,
  weixinInstanceKey,
  weixinInstanceSlug,
  weixinInstanceTitle,
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
  assert.deepEqual(diffChannel(telegram, { ...d, groupPolicy: "open", replyToMessage: true, inlineKeyboards: true, pollTimeout: "20" }), {
    groupPolicy: "open",
    replyToMessage: true,
    inlineKeyboards: true,
    pollTimeout: 20,
  });
  assert.deepEqual(diffChannel(telegram, { ...d, enabled: false, baseUrl: "https://ignored.test" }), { enabled: false });
});

test("parseAllowFrom", () => {
  assert.deepEqual(parseAllowFrom(""), []);
  assert.deepEqual(parseAllowFrom("a,b , a\nc"), ["a", "b", "c"]);
});

test("WeChat instance names and channel partitions", () => {
  assert.equal(weixinInstanceSlug("weixin"), "");
  assert.equal(weixinInstanceSlug("weixin.work"), "work");
  assert.equal(weixinInstanceTitle("weixin"), "Default");
  assert.equal(weixinInstanceTitle("weixin.work"), "work");
  assert.equal(weixinInstanceKey("work"), "weixin.work");
  assert.deepEqual(parseWeixinInstanceInput(" work "), { slug: "work", key: "weixin.work" });
  assert.deepEqual(parseWeixinInstanceInput("WEIXIN.Personal_2"), { slug: "personal_2", key: "weixin.personal_2" });
  assert.equal(parseWeixinInstanceInput("").error?.key, "channels.validation.weixinInstanceEmpty");
  assert.equal(parseWeixinInstanceInput("weixin").error?.key, "channels.validation.weixinInstanceReserved");
  for (const bad of ["two.words", "2work", "work!", "a".repeat(33)]) {
    assert.equal(parseWeixinInstanceInput(bad).error?.key, "channels.validation.weixinInstanceInvalid", bad);
  }

  const work = { ...weixin, name: "weixin.work" };
  const other = { ...weixin, name: "custom" };
  const grouped = partitionChannelConfigs([work, other, telegram, weixin]);
  assert.equal(grouped.telegram, telegram);
  assert.deepEqual(grouped.weixinInstances.map((c) => c.name), ["weixin", "weixin.work"]);
  assert.deepEqual(grouped.other, [other]);
});

test("checkToken", () => {
  assert.deepEqual(checkToken("telegram", ""), null);
  assert.deepEqual(checkToken("telegram", "123456:AA_b-c"), null);
  assert.deepEqual(checkToken("telegram", "***"), { key: "channels.validation.tokenRedacted" });
  assert.deepEqual(checkToken("telegram", telegram.tokenHint), { key: "channels.validation.tokenRedacted" });
  assert.deepEqual(checkToken("telegram", "a b"), { key: "channels.validation.tokenSpaces" });
  assert.deepEqual(checkToken("telegram", "nocolon"), { key: "channels.validation.telegramTokenShape" });
  assert.deepEqual(checkToken("weixin", "nocolon"), null);
});

test("checkChannel", () => {
  const d = channelDraft(telegram);
  assert.deepEqual(checkChannel(telegram, d), null);
  assert.deepEqual(checkChannel(telegram, { ...d, clearToken: true }), { key: "channels.validation.telegramNeedsToken" });
  assert.deepEqual(checkChannel(telegram, { ...d, clearToken: true, enabled: false }), null);
  assert.deepEqual(checkChannel({ ...telegram, env_overrides: ["token"] }, { ...d, clearToken: true }), null);
  assert.deepEqual(checkChannel(telegram, { ...d, apiBase: "ftp://x" }), { key: "channels.validation.apiBaseUrl" });
  assert.deepEqual(checkChannel(telegram, { ...d, pollTimeout: "-1" }), { key: "channels.validation.pollTimeout", vars: { max: 3600 } });
  assert.deepEqual(checkChannel(weixin, { ...channelDraft(weixin), enabled: true }), null);
  assert.deepEqual(checkChannel(weixin, { ...channelDraft(weixin), baseUrl: "https://u:p@x.test" }), { key: "channels.validation.baseUrlCredentials" });
});

test("tokenStatus and channelState", () => {
  assert.equal(tokenStatus(telegram), "Set: 123456:••••abcd");
  assert.equal(tokenStatus({ ...telegram, hasToken: false, tokenHint: "", env_overrides: ["token"] }), "Not set (overridden by the environment)");
  assert.equal(tokenStatus(weixin), "From 'wally channels login weixin'");
  assert.equal(tokenStatus({ ...weixin, name: "weixin.work", login: { loggedIn: true, statePath: "x", cliCommand: "wally channels login weixin.work", loginHint: "" } }), "From 'wally channels login weixin.work'");
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

const loggedOut: WeixinLogin = {
  loggedIn: false,
  statePath: "/tmp/nb/state/weixin/account.json",
  cliCommand: "wally channels login weixin --config /tmp/nb/config.json",
  loginHint: "Not logged in.",
};

test("loginStatus and loginDetails describe the saved WeChat login", () => {
  assert.deepEqual(loginStatus(loggedOut), { kind: "", text: "Not logged in" });
  assert.deepEqual(loginDetails(loggedOut), []);
  const on: WeixinLogin = { ...loggedOut, loggedIn: true, botId: "bot@im.bot", userId: "u@im.wechat", baseUrl: "https://a.example" };
  assert.deepEqual(loginStatus(on), { kind: "ok", text: "Login saved" });
  assert.deepEqual(loginDetails(on), [
    ["Bot ID", "bot@im.bot"],
    ["User ID", "u@im.wechat"],
    ["Assigned base URL", "https://a.example"],
  ]);
  assert.deepEqual(loginDetails({ ...on, botId: "", baseUrl: undefined }), [["User ID", "u@im.wechat"]]);
  const bad = loginStatus({ ...loggedOut, error: "account.json is corrupt" });
  assert.equal(bad.kind, "error");
  assert.match(bad.text, /corrupt/);
});

test("forceLoginCommand and weixinLogin", () => {
  assert.equal(forceLoginCommand(loggedOut.cliCommand), "wally channels login weixin --config /tmp/nb/config.json --force");
  assert.equal(weixinLogin([telegram]), undefined);
  assert.equal(weixinLogin([telegram, { ...telegram, name: "weixin", login: loggedOut }]), loggedOut);
});

test("canClearLogin, clearLoginPrompt and withLogin", () => {
  assert.equal(canClearLogin(loggedOut), false);
  assert.equal(canClearLogin({ ...loggedOut, loggedIn: true }), true);
  assert.equal(canClearLogin({ ...loggedOut, error: "corrupt" }), true);
  const p = clearLoginPrompt({ ...loggedOut, statePath: "/tmp/nb/state/weixin/account.json" }, false);
  assert.match(p, /account\.json/);
  assert.doesNotMatch(p, /restart/);
  assert.match(clearLoginPrompt(loggedOut, true), /restarts/);

  const before: ChannelConfig = { ...weixin, allowFrom: ["wx_user"], login: { ...loggedOut, loggedIn: true, botId: "b" } };
  const after: ChannelConfig = { ...weixin, allowFrom: [], hasLoginState: false, login: loggedOut, restart_required: true, running: true };
  const merged = withLogin(before, after);
  assert.equal(merged.login, loggedOut);
  assert.equal(merged.hasLoginState, false);
  assert.equal(merged.restart_required, true);
  assert.deepEqual(merged.allowFrom, ["wx_user"]);
  assert.deepEqual(channelDraft(merged), channelDraft(before));
});
