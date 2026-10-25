import assert from "node:assert/strict";
import { test } from "node:test";
import { chatFrame, parseFrame } from "./protocol.ts";

test("parseFrame reads thinking frames", () => {
  assert.deepEqual(parseFrame('{"type":"thinking","content":"hmm"}'), { type: "thinking", content: "hmm" });
  assert.deepEqual(parseFrame('{"type":"thinking_end"}'), { type: "thinking_end" });
  assert.equal(parseFrame("not json"), null);
  assert.equal(parseFrame('{"content":"no type"}'), null);
});

test("chatFrame", () => {
  assert.deepEqual(chatFrame("hi"), { type: "chat", content: "hi" });
  assert.deepEqual(chatFrame("hi", []), { type: "chat", content: "hi" });
  const media = [
    { url: "/ui/media/uploads/a.png", name: "a.png", kind: "image" as const, extra: 1 },
    { url: "/ui/media/uploads/b.pdf", name: "b.pdf", kind: "file" as const },
  ];
  assert.deepEqual(JSON.parse(JSON.stringify(chatFrame("", media))), {
    type: "chat",
    content: "",
    media: [
      { url: "/ui/media/uploads/a.png", name: "a.png", kind: "image" },
      { url: "/ui/media/uploads/b.pdf", name: "b.pdf", kind: "file" },
    ],
  });
});
