// Run with `npm test` (Node's built-in runner; Node strips the types).
import assert from "node:assert/strict";
import { test } from "node:test";
import { emptyTranscript, reduce, type Action, type Transcript } from "./transcript.ts";

function run(...actions: Action[]): Transcript {
  return actions.reduce(reduce, emptyTranscript);
}

const shown = (t: Transcript) => t.messages.map((m) => [m.kind, m.content, !!m.streaming]);

test("history keeps user and assistant texts only", () => {
  const t = run({
    type: "history",
    messages: [
      { role: "user", content: "hi" },
      { role: "tool", content: "x" },
      { role: "assistant", content: "hello" },
    ],
  });
  assert.deepEqual(shown(t), [
    ["user", "hi", false],
    ["assistant", "hello", false],
  ]);
  assert.equal(t.waiting, 0);
});

test("deltas stream into one bubble that the message replaces", () => {
  let t = run({ type: "sent", content: "q" }, { type: "progress", content: "exec" });
  assert.equal(t.waiting, 1);
  assert.equal(t.progress, "exec");
  t = reduce(t, { type: "delta", content: "Hel" });
  t = reduce(t, { type: "delta", content: "lo" });
  assert.deepEqual(shown(t), [
    ["user", "q", false],
    ["assistant", "Hello", true],
  ]);
  assert.ok(t.pendingHidden);
  t = reduce(t, { type: "progress", content: "web_fetch" });
  assert.ok(!t.pendingHidden, "progress shows below streamed text");
  t = reduce(t, { type: "message", content: "Hello!" });
  assert.deepEqual(shown(t), [
    ["user", "q", false],
    ["assistant", "Hello!", false],
  ]);
  assert.equal(t.waiting, 0);
  assert.equal(t.live, null);
  assert.equal(t.progress, null);
});

test("a message without deltas appends a bubble", () => {
  const t = run({ type: "sent", content: "a" }, { type: "sent", content: "b" }, { type: "message", content: "A" });
  assert.equal(t.waiting, 1);
  assert.deepEqual(shown(t).at(-1), ["assistant", "A", false]);
});

test("frames from another tab count as a reply due", () => {
  const t = run({ type: "delta", content: "x" });
  assert.equal(t.waiting, 1);
  assert.equal(reduce(t, { type: "message", content: "xy" }).waiting, 0);
});

test("disconnect settles the turn; history replaces the list", () => {
  let t = run({ type: "sent", content: "q" }, { type: "delta", content: "part" }, { type: "disconnected" });
  assert.equal(t.waiting, 0);
  assert.equal(t.live, null);
  assert.ok(!t.messages.some((m) => m.streaming));
  t = reduce(t, { type: "history", messages: [{ role: "user", content: "q" }] });
  assert.deepEqual(shown(t), [["user", "q", false]]);
  // ids stay unique across replacements
  assert.ok(t.messages[0].id > 2);
});

test("errors are shown", () => {
  assert.deepEqual(shown(run({ type: "error", content: "boom" })), [["error", "boom", false]]);
});
