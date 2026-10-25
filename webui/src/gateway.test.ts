import assert from "node:assert/strict";
import { test } from "node:test";
import type { GatewayInfo } from "./api.ts";
import { type GatewayPoll, waitRestarted } from "./gateway.ts";

const before: GatewayInfo = { pid: 10, launch_mode: "background", lifetime: "explicit", restart_available: true, restarting: true };
const pause = async () => {};

function polls(...seq: (GatewayPoll | Error)[]): () => Promise<GatewayPoll> {
  let i = 0;
  return async () => {
    const v = seq[Math.min(i++, seq.length - 1)];
    if (v instanceof Error) throw v;
    return v;
  };
}

test("waitRestarted waits through the old process and the gap for a new pid", async () => {
  assert.equal(await waitRestarted(before, polls(before, null, new Error("down"), { ...before, pid: 11 }), pause), "restarted");
});

test("waitRestarted stops for sign-in", async () => {
  assert.equal(await waitRestarted(before, polls(null, "signin"), pause), "signin");
});

test("waitRestarted gives up", async () => {
  assert.equal(await waitRestarted(before, polls(before), pause, 3), "timeout");
  assert.equal(await waitRestarted(before, polls(null), pause, 3), "timeout");
});
