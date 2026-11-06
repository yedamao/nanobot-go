import assert from "node:assert/strict";
import { test } from "node:test";
import { clipboardFiles, nameClipboardFile } from "./clipboard.ts";

const now = new Date(2026, 10, 6, 14, 5, 9);

test("nameClipboardFile gives unnamed files a timestamped MIME extension", () => {
  const file = new File(["document"], "", { type: "application/pdf", lastModified: 123 });
  const named = nameClipboardFile(file, now);

  assert.equal(named.name, "paste-20261106-140509.pdf");
  assert.equal(named.type, "application/pdf");
  assert.equal(named.lastModified, 123);
  assert.equal(named.size, file.size);
});

test("nameClipboardFile preserves an existing filename", () => {
  const file = new File([], "archive.zip", { type: "application/zip" });
  assert.equal(nameClipboardFile(file), file);
});

test("clipboardFiles selects non-image file items when clipboard also contains text", () => {
  const pdf = new File([], "", { type: "application/pdf" });
  const data = {
    items: [
      { kind: "string", type: "text/plain", getAsFile: () => null },
      { kind: "file", type: "application/pdf", getAsFile: () => pdf },
    ],
    files: [pdf],
  } as unknown as DataTransfer;

  const files = clipboardFiles(data, now);
  assert.equal(files.length, 1);
  assert.equal(files[0]?.name, "paste-20261106-140509.pdf");
});

test("clipboardFiles keeps image paste working", () => {
  const image = new File([], "", { type: "image/svg+xml" });
  const data = {
    items: [{ kind: "file", type: "image/svg+xml", getAsFile: () => image }],
    files: [image],
  } as unknown as DataTransfer;

  const files = clipboardFiles(data, now);
  assert.equal(files.length, 1);
  assert.equal(files[0]?.name, "paste-20261106-140509.svg");
});

test("clipboardFiles ignores text-only clipboard data", () => {
  const data = {
    items: [{ kind: "string", type: "text/plain", getAsFile: () => null }],
    files: [],
  } as unknown as DataTransfer;

  assert.deepEqual(clipboardFiles(data), []);
});

test("clipboardFiles falls back to data.files", () => {
  const file = new File([], "notes.txt", { type: "text/plain" });
  const data = { items: [], files: [file] } as unknown as DataTransfer;

  assert.deepEqual(clipboardFiles(data), [file]);
});

test("nameClipboardFile uses .bin for unknown MIME types", () => {
  const file = new File([], "", { type: "application/x-custom" });
  assert.equal(nameClipboardFile(file, now).name, "paste-20261106-140509.bin");
});
