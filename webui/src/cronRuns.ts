import { enT, type Translator } from "./i18n/translate.ts";

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

/** runStatusText is a run status for display: ok, error and skipped
 * translated, other values as they are, none as an em dash. */
export function runStatusText(status: string | undefined, t: Translator = enT): string {
  switch (status) {
    case "ok":
      return t("cron.status.ok");
    case "error":
      return t("cron.status.error");
    case "skipped":
      return t("cron.status.skipped");
    case "":
    case undefined:
      return "—";
    default:
      return status;
  }
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

/** runReply returns a run's reply for display, or "" when it has none. */
export function runReply(run: { reply?: string }): string {
  return run.reply?.trim() ?? "";
}
