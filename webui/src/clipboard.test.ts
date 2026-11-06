import assert from "node:assert/strict";
import { test } from "node:test";
import { clipboardImages, nameClipboardImage } from "./clipboard.ts";

test("nameClipboardImage gives unnamed clipboard images a timestamped MIME extension", () => {
  const image = new File(["pixels"], "", { type: "image/png", lastModified: 123 });
  const named = nameClipboardImage(image, new Date(2026, 10, 6, 14, 5, 9));

  assert.equal(named.name, "paste-20261106-140509.png");
  assert.equal(named.type, "image/png");
  assert.equal(named.lastModified, 123);
  assert.equal(named.size, image.size);
});

test("nameClipboardImage preserves an existing filename", () => {
  const image = new File([], "screenshot.jpeg", { type: "image/jpeg" });
  assert.equal(nameClipboardImage(image), image);
});

test("nameClipboardImage normalizes structured MIME subtypes", () => {
  const image = new File([], "", { type: "image/svg+xml" });
  assert.equal(nameClipboardImage(image, new Date(2026, 0, 2, 3, 4, 5)).name, "paste-20260102-030405.svg");
});

test("clipboardImages selects image items when clipboard also contains text", () => {
  const image = new File([], "", { type: "image/png" });
  const data = {
    items: [
      { kind: "string", type: "text/plain", getAsFile: () => null },
      { kind: "file", type: "image/png", getAsFile: () => image },
    ],
    files: [image],
  } as unknown as DataTransfer;

  const images = clipboardImages(data, new Date(2026, 0, 2, 3, 4, 5));
  assert.equal(images.length, 1);
  assert.equal(images[0]?.name, "paste-20260102-030405.png");
});

test("clipboardImages ignores text-only clipboard data", () => {
  const data = {
    items: [{ kind: "string", type: "text/plain", getAsFile: () => null }],
    files: [],
  } as unknown as DataTransfer;

  assert.deepEqual(clipboardImages(data), []);
});
