import { useEffect, useState } from "react";
import { type GatewayInfo, getGateway, restartGateway } from "./api";
import { pollGateway, waitRestarted } from "./gateway";

// RestartGateway sits under a "restart the gateway to apply" note: a
// Restart gateway button when the gateway is background-managed, else
// why the page cannot restart it. After a restart it reloads the page
// once the new gateway answers.
export function RestartGateway() {
  const [info, setInfo] = useState<GatewayInfo | null>(null);
  const [state, setState] = useState<{ kind: "" | "busy" | "error"; text: string }>({ kind: "", text: "" });

  useEffect(() => {
    getGateway()
      .then(setInfo)
      .catch(() => setInfo(null));
  }, []);

  if (!info) return null;
  if (!info.restart_available) {
    return info.restart_unavailable_reason ? <p className="status-note">{info.restart_unavailable_reason}</p> : null;
  }

  const restart = async () => {
    if (!confirm("Restart the gateway?\n\nIt stops and starts again; this page may briefly disconnect.")) return;
    setState({ kind: "busy", text: "Restarting the gateway…" });
    try {
      await restartGateway();
    } catch (err) {
      setState({ kind: "error", text: "Not restarted: " + (err as Error).message });
      return;
    }
    const outcome = await waitRestarted(info, pollGateway, () => new Promise((r) => setTimeout(r, 1000)));
    if (outcome === "timeout") {
      setState({
        kind: "error",
        text: "The gateway has not come back yet; check 'nanobot gateway status' and 'nanobot gateway logs'.",
      });
      return;
    }
    location.reload();
  };

  return (
    <p className="restart-gateway">
      <button type="button" onClick={() => void restart()} disabled={state.kind === "busy" || info.restarting}>
        Restart gateway
      </button>{" "}
      {state.text && (
        <span className={state.kind === "error" ? "page-error" : "page-note"} role="status">
          {state.text}
        </span>
      )}
    </p>
  );
}
