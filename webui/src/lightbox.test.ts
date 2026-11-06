import assert from "node:assert/strict";
import test from "node:test";
import { wrapIndex } from "./lightbox.ts";

test("wrapIndex wraps image navigation at both ends", () => {
  assert.equal(wrapIndex(3, 3), 0);
  assert.equal(wrapIndex(-1, 3), 2);
  assert.equal(wrapIndex(1, 3), 1);
});
