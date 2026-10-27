import assert from "node:assert/strict";
import { test } from "node:test";
import { bindingKey, bindingLabel, bindingOptions, bindingWrite, optionLabel, sameBinding } from "./cronBinding.ts";

const def = { channel: "websocket", chatId: "cron" };

test("bindingLabel and optionLabel", () => {
  assert.equal(bindingLabel({ channel: "telegram", chatId: "42" }), "telegram · 42");
  assert.equal(optionLabel({ channel: "telegram", chatId: "42", title: "Daily" }), "telegram · 42 — Daily");
  assert.equal(optionLabel(def), "websocket · cron");
});

test("bindingKey is unambiguous", () => {
  assert.notEqual(bindingKey({ channel: "a", chatId: "b c" }), bindingKey({ channel: "a b", chatId: "c" }));
});

test("sameBinding trims and handles missing", () => {
  assert.ok(sameBinding(undefined, undefined));
  assert.ok(!sameBinding(def, undefined));
  assert.ok(sameBinding({ channel: " websocket", chatId: "cron " }, def));
});

test("bindingOptions puts default and current first without duplicates", () => {
  const targets = [
    { channel: "telegram", chatId: "42", title: "Daily" },
    { channel: "websocket", chatId: "cron", title: "Cron chat" },
  ];
  assert.deepEqual(bindingOptions(targets, def), [
    { channel: "websocket", chatId: "cron", title: "Cron chat" },
    { channel: "telegram", chatId: "42", title: "Daily" },
  ]);
  const current = { channel: "weixin", chatId: "gone" };
  assert.deepEqual(
    bindingOptions(targets, def, current).map((o) => o.chatId),
    ["cron", "gone", "42"],
  );
  assert.deepEqual(bindingOptions([], undefined), []);
});

test("bindingWrite omits the default and an unchanged binding", () => {
  assert.equal(bindingWrite(undefined, def), undefined);
  assert.equal(bindingWrite(def, def), undefined);
  assert.equal(bindingWrite({ channel: "telegram", chatId: "42" }, { channel: "telegram", chatId: "42" }), undefined);
  assert.deepEqual(bindingWrite({ channel: " telegram ", chatId: " 42" }, def), { channel: "telegram", chatId: "42" });
  // A job with no binding gets one.
  assert.deepEqual(bindingWrite(def, undefined), def);
});

test("bindingWrite validates custom input", () => {
  assert.equal(bindingWrite({ channel: "telegram", chatId: " " }, def), "cron.validation.bindingRequired");
  assert.equal(bindingWrite({ channel: "", chatId: "42" }, def), "cron.validation.bindingRequired");
  assert.equal(bindingWrite({ channel: "tele:gram", chatId: "42" }, def), "cron.validation.bindingColon");
});
