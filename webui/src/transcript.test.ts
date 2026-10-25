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

test("a message's media is kept on its bubble", () => {
  const media = [{ url: "/ui/media/c.png", name: "c.png", kind: "image" as const }];
  let t = run({ type: "sent", content: "q" }, { type: "message", content: "a", media });
  assert.deepEqual(t.messages[1].media, media);
  t = run({ type: "delta", content: "p" }, { type: "message", content: "a", media });
  assert.deepEqual(t.messages[0].media, media);
  t = run({ type: "message", content: "a", media: [] });
  assert.equal(t.messages[0].media, undefined);
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

test("thinking streams into a panel apart from the reply", () => {
  let t = run({ type: "sent", content: "q" }, { type: "thinking", content: "Hm" }, { type: "thinking", content: "m." });
  assert.deepEqual(shown(t), [
    ["user", "q", false],
    ["thinking", "Hmm.", true],
  ]);
  assert.ok(t.pendingHidden, "the pending bubble hides while thinking streams");
  t = reduce(t, { type: "thinking_end" });
  assert.deepEqual(shown(t).at(-1), ["thinking", "Hmm.", false]);
  assert.ok(!t.pendingHidden);
  t = reduce(t, { type: "delta", content: "Hi" });
  t = reduce(t, { type: "message", content: "Hi!" });
  // The thinking stays above the reply, collapsed; it is not in the reply.
  assert.deepEqual(shown(t), [
    ["user", "q", false],
    ["thinking", "Hmm.", false],
    ["assistant", "Hi!", false],
  ]);
  assert.equal(t.thinking, null);
  // History does not bring it back.
  t = reduce(t, {
    type: "history",
    messages: [
      { role: "user", content: "q" },
      { role: "assistant", content: "Hi!" },
    ],
  });
  assert.ok(!t.messages.some((m) => m.kind === "thinking"));
});

test("a turn's thinking runs share one panel; the next turn gets its own", () => {
  let t = run(
    { type: "sent", content: "q" },
    { type: "thinking", content: "Need a tool." },
    { type: "thinking_end" },
    { type: "progress", content: "exec" },
    { type: "thinking", content: "\n\nGot it." },
  );
  assert.deepEqual(shown(t).at(-1), ["thinking", "Need a tool.\n\nGot it.", true]);
  t = reduce(t, { type: "message", content: "Done" });
  assert.ok(!t.messages.some((m) => m.streaming), "the reply settles thinking without thinking_end");
  t = reduce(reduce(t, { type: "sent", content: "q2" }), { type: "thinking", content: "\n\nAgain." });
  assert.deepEqual(shown(t).at(-1), ["thinking", "Again.", true]);
  assert.equal(t.messages.filter((m) => m.kind === "thinking").length, 2);
});

test("disconnect settles thinking", () => {
  const t = run({ type: "thinking", content: "x" }, { type: "disconnected" });
  assert.equal(t.thinking, null);
  assert.ok(!t.messages.some((m) => m.streaming));
});
