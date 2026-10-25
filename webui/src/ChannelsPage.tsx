import { useCallback, useEffect, useState } from "react";
import {
  approvePairing,
  denyPairing,
  getPairing,
  getStatus,
  revokePairing,
  type PairingView,
  type Status,
} from "./api";

export function ChannelsPage() {
  const [status, setStatus] = useState<Status | null>(null);
  const [pairing, setPairing] = useState<PairingView | null>(null);
  const [error, setError] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    setError("");
    try {
      const [st, p] = await Promise.all([getStatus(), getPairing()]);
      setStatus(st);
      setPairing(p);
    } catch (err) {
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const act = async (label: string, fn: () => Promise<void>) => {
    setBusy(true);
    setNote("");
    try {
      await fn();
      setNote(label);
      await load();
    } catch (err) {
      setNote(label + " failed: " + (err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const approvedRows: { channel: string; sender: string }[] = [];
  if (pairing) {
    for (const [channel, ids] of Object.entries(pairing.approved)) {
      for (const sender of ids) approvedRows.push({ channel, sender });
    }
  }

  return (
    <section className="page" aria-labelledby="channels-heading">
      <header className="page-header">
        <h2 id="channels-heading">Channels</h2>
        <button type="button" onClick={() => void load()} disabled={busy}>
          Refresh
        </button>
      </header>
      {error && <p className="page-error">Cannot load ({error}).</p>}
      {note && <p className="page-note">{note}</p>}

      <h3>Configured</h3>
      {!status && !error && <p className="page-empty">Loading…</p>}
      {status && (status.channels?.length ?? 0) === 0 && <p className="page-empty">No channels configured.</p>}
      {status && (status.channels?.length ?? 0) > 0 && (
        <ul className="page-list">
          {(status.channels ?? []).map((c) => (
            <li key={c.name}>
              <strong>{c.name}</strong> — {c.running ? "running" : c.enabled ? "enabled, not running" : "disabled"}
            </li>
          ))}
        </ul>
      )}

      <h3>Pending pairing</h3>
      {pairing && pairing.pending.length === 0 && <p className="page-empty">No pending requests.</p>}
      {pairing && pairing.pending.length > 0 && (
        <table className="page-table">
          <thead>
            <tr>
              <th>Code</th>
              <th>Channel</th>
              <th>Sender</th>
              <th>Expires</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {pairing.pending.map((p) => (
              <tr key={p.code}>
                <td>
                  <code>{p.code}</code>
                </td>
                <td>{p.channel}</td>
                <td>{p.sender_id}</td>
                <td>{new Date(p.expires_at).toLocaleString()}</td>
                <td className="page-actions">
                  <button type="button" disabled={busy} onClick={() => void act("Approved " + p.code, () => approvePairing(p.code))}>
                    Approve
                  </button>
                  <button type="button" disabled={busy} onClick={() => void act("Denied " + p.code, () => denyPairing(p.code))}>
                    Deny
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h3>Approved senders</h3>
      {approvedRows.length === 0 && <p className="page-empty">None yet.</p>}
      {approvedRows.length > 0 && (
        <table className="page-table">
          <thead>
            <tr>
              <th>Channel</th>
              <th>Sender</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {approvedRows.map((r) => (
              <tr key={r.channel + ":" + r.sender}>
                <td>{r.channel}</td>
                <td>{r.sender}</td>
                <td className="page-actions">
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() =>
                      void act("Revoked " + r.sender, () => revokePairing(r.channel, r.sender))
                    }
                  >
                    Revoke
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
