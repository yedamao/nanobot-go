import assert from "node:assert/strict";
import { test } from "node:test";
import { attachmentText, imagesInText, isMediaImage, isMediaURL, mediaRefs } from "./media.ts";

test("isMediaURL", () => {
  assert.ok(isMediaURL("/ui/media/a.png"));
  assert.ok(isMediaURL("/ui/media/out/my%20file.pdf"));
  for (const u of [
    "/ui/media/",
    "/ui/media/../x.png",
    "/ui/media/a/%2e%2e/x.png",
    "/ui/media/.env",
    "/ui/media//x.png",
    "/ui/media/a\\b.png",
    "/ui/media/a.png?x",
    "/ui/media/a.png#x",
    "/ui/media/%E0%A4%A.png",
    "https://evil/ui/media/a.png",
    "/ui/chats",
  ]) {
    assert.equal(isMediaURL(u), false, u);
  }
  assert.ok(isMediaImage("/ui/media/a.JPG"));
  assert.equal(isMediaImage("/ui/media/a.pdf"), false);
});

test("mediaRefs keeps media URLs and remote links", () => {
  assert.deepEqual(
    mediaRefs([
      { url: "/ui/media/out/c.png", name: "c.png", kind: "file" },
      { url: "/ui/media/r%20s.pdf" },
      { url: "https://cdn.example/x.png", name: "x", kind: "image" },
      { url: "http://cdn.example/a%20b.pdf?sig=1" },
      { url: "https://cdn.example/" },
      { url: "javascript:alert(1)" },
      { url: "data:image/png;base64,AAAA" },
      { url: "/ui/chats" },
      null,
      "str",
    ]),
    [
      { url: "/ui/media/out/c.png", name: "c.png", kind: "image" },
      { url: "/ui/media/r%20s.pdf", name: "r s.pdf", kind: "file" },
      { url: "https://cdn.example/x.png", name: "x", kind: "link" },
      { url: "http://cdn.example/a%20b.pdf?sig=1", name: "a b.pdf", kind: "link" },
      { url: "https://cdn.example/", name: "cdn.example", kind: "link" },
    ],
  );
  assert.deepEqual(mediaRefs(undefined), []);
  assert.deepEqual(mediaRefs({ url: "/ui/media/a.png" }), []);
});

test("attachmentText", () => {
  assert.equal(attachmentText({ path: "uploads/ab-c.png", name: "c.png", kind: "image" }), "![c.png](uploads/ab-c.png)");
  assert.equal(attachmentText({ path: "uploads/ab-r.pdf", name: "a]b[.pdf", kind: "file" }), "[a_b_.pdf](uploads/ab-r.pdf)");
});

test("imagesInText", () => {
  assert.deepEqual(imagesInText('see ![a](/ui/media/a.png) and ![](/ui/media/b.gif "t") ![a](/ui/media/a.png)'), [
    { url: "/ui/media/a.png", name: "a", kind: "image" },
    { url: "/ui/media/b.gif", name: "b.gif", kind: "image" },
  ]);
  assert.deepEqual(imagesInText("![x](uploads/a.png) ![y](https://e/x.png) ![z](/ui/media/r.pdf)"), []);
});
