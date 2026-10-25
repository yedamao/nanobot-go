// Waiting for the gateway to come back after a restart from the page.

import type { GatewayInfo } from "./api";

// A poll of GET /ui/gateway: the body, "signin" on a 401 (the new
// gateway forgot this browser's session), or null while it is down.
export type GatewayPoll = GatewayInfo | "signin" | null;

export type RestartOutcome = "restarted" | "signin" | "timeout";

// waitRestarted polls until a gateway other than the process before
// answers (or sign-in is needed), at most attempts times, pausing
// between polls.
export async function waitRestarted(
  before: GatewayInfo,
  poll: () => Promise<GatewayPoll>,
  pause: () => Promise<void>,
  attempts = 60,
): Promise<RestartOutcome> {
  for (let i = 0; i < attempts; i++) {
    await pause();
    const got = await poll().catch(() => null);
    if (got === "signin") return "signin";
    if (got && got.pid !== before.pid) return "restarted";
  }
  return "timeout";
}

// pollGateway is one GET /ui/gateway that never throws.
export async function pollGateway(): Promise<GatewayPoll> {
  try {
    const resp = await fetch("gateway", { cache: "no-store" });
    if (resp.status === 401) return "signin";
    return resp.ok ? ((await resp.json()) as GatewayInfo) : null;
  } catch {
    return null;
  }
}
