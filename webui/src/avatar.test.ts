import assert from "node:assert/strict";
import { test } from "node:test";
import { AVATAR_COLORS, avatarColor, avatarLetter } from "./avatar.ts";

test("avatarColor is stable and from the palette", () => {
  assert.equal(avatarColor("abc"), avatarColor("abc"));
  assert.ok((AVATAR_COLORS as readonly string[]).includes(avatarColor("abc")));
  assert.ok((AVATAR_COLORS as readonly string[]).includes(avatarColor("")));
  const seen = new Set(Array.from({ length: 50 }, (_, i) => avatarColor(`chat-${i}`)));
  assert.ok(seen.size > 1);
});

test("avatarLetter takes the first grapheme", () => {
  assert.equal(avatarLetter("hello"), "H");
  assert.equal(avatarLetter("  world"), "W");
  assert.equal(avatarLetter("你好"), "你");
  assert.equal(avatarLetter("👍🏽 nice"), "👍🏽");
  assert.equal(avatarLetter(""), "?");
  assert.equal(avatarLetter("   "), "?");
  assert.equal(avatarLetter(undefined), "?");
});
