/** formatDuration renders a run's duration compactly: 850ms, 12.3s, 4m 05s. */
export function formatDuration(ms?: number): string {
  if (ms === undefined || !Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return Math.round(ms) + "ms";
  if (ms < 60_000) return (ms / 1000).toFixed(1).replace(/\.0$/, "") + "s";
  const total = Math.round(ms / 1000);
  const m = Math.floor(total / 60);
  const s = total % 60;
  if (m < 60) return m + "m " + String(s).padStart(2, "0") + "s";
  return Math.floor(m / 60) + "h " + String(m % 60).padStart(2, "0") + "m";
}

/** runStatusState maps a run status to a badge data-state. */
export function runStatusState(status: string): string {
  switch (status) {
    case "ok":
      return "connected";
    case "error":
      return "failed";
    default:
      return status;
  }
}
