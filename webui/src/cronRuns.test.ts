import assert from "node:assert/strict";
import { test } from "node:test";
import { formatDuration, runReply, runStatusState } from "./cronRuns.ts";

test("formatDuration", () => {
  assert.equal(formatDuration(undefined), "—");
  assert.equal(formatDuration(0), "0ms");
  assert.equal(formatDuration(850), "850ms");
  assert.equal(formatDuration(1000), "1s");
  assert.equal(formatDuration(12_340), "12.3s");
  assert.equal(formatDuration(245_000), "4m 05s");
  assert.equal(formatDuration(3_720_000), "1h 02m");
});

test("runStatusState maps statuses to badge states", () => {
  assert.equal(runStatusState("ok"), "connected");
  assert.equal(runStatusState("error"), "failed");
  assert.equal(runStatusState("skipped"), "skipped");
});

test("runReply trims and defaults to empty", () => {
  assert.equal(runReply({}), "");
  assert.equal(runReply({ reply: "  \n " }), "");
  assert.equal(runReply({ reply: "\nTime to stretch!\n" }), "Time to stretch!");
});
