import { useCallback, useEffect, useState } from "react";
import { type GatewayInfo, getGateway, restartGateway } from "./api";
import { pollGateway, waitRestarted } from "./gateway";
import { Icon } from "./Icon";
import { useT } from "./i18n";

type RestartState = { kind: "" | "busy" | "error"; text: string };

// useRestartGateway loads GET /ui/gateway and restarts the gateway on
// request: it confirms, posts the restart, waits for a new gateway
// process to answer and reloads the page.
export function useRestartGateway() {
  const t = useT();
  const [info, setInfo] = useState<GatewayInfo | null>(null);
  const [state, setState] = useState<RestartState>({ kind: "", text: "" });

  useEffect(() => {
    getGateway()
      .then(setInfo)
      .catch(() => setInfo(null));
  }, []);

  const restart = useCallback(async () => {
    if (!info?.restart_available) return;
    if (!confirm(t("restart.confirm"))) return;
    setState({ kind: "busy", text: t("restart.busy") });
    try {
      await restartGateway();
    } catch (err) {
      setState({ kind: "error", text: t("restart.failed", { message: (err as Error).message }) });
      return;
    }
    const outcome = await waitRestarted(info, pollGateway, () => new Promise((r) => setTimeout(r, 1000)));
    if (outcome === "timeout") {
      setState({ kind: "error", text: t("restart.timeout") });
      return;
    }
    location.reload();
  }, [info, t]);

  const available = !!info?.restart_available;
  const busy = state.kind === "busy" || !!info?.restarting;
  return { info, available, busy, state, restart };
}

// RestartButton is a Restart gateway button: disabled, titled with the
// reason, when the page cannot restart this gateway. Its outcome shows
// in a status line after it unless quiet.
export function RestartButton({ className = "btn", label }: { className?: string; label?: string }) {
  const t = useT();
  const { info, available, busy, state, restart } = useRestartGateway();
  return (
    <>
      <button
        type="button"
        className={className}
        disabled={!available || busy}
        title={!info ? undefined : available ? t("restart.title") : info.restart_unavailable_reason || t("restart.unavailable")}
        onClick={() => void restart()}
      >
        <Icon name="power" />
        <span>{label ?? t("restart.button")}</span>
      </button>
      {state.text && (
        <span className={"restart-note " + (state.kind === "error" ? "page-error" : "page-note")} role="status">
          {state.text}
        </span>
      )}
    </>
  );
}

// RestartGateway sits under a "restart the gateway to apply" note: a
// Restart gateway button when the gateway is background-managed, else
// why the page cannot restart it. After a restart it reloads the page
// once the new gateway answers.
export function RestartGateway() {
  const t = useT();
  const { info, available, busy, state, restart } = useRestartGateway();

  if (!info) return null;
  if (!available) {
    return info.restart_unavailable_reason ? <p className="status-note">{info.restart_unavailable_reason}</p> : null;
  }

  return (
    <p className="restart-gateway">
      <button type="button" className="btn sm" onClick={() => void restart()} disabled={busy}>
        <Icon name="power" />
        {t("restart.button")}
      </button>
      {state.text && (
        <span className={state.kind === "error" ? "page-error" : "page-note"} role="status">
          {state.text}
        </span>
      )}
    </p>
  );
}
