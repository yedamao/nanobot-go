import assert from "node:assert/strict";
import { test } from "node:test";
import { chatFrame } from "./protocol.ts";

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
