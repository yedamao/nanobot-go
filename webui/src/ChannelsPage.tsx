import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  approvePairing,
  denyPairing,
  getPairing,
  getStatus,
  listChannelConfigs,
  revokePairing,
  saveChannelConfig,
  testChannelConfig,
  type ChannelConfig,
  type PairingView,
  type Status,
} from "./api";
import {
  canTest,
  channelDraft,
  channelState,
  checkChannel,
  checkToken,
  diffChannel,
  testNote,
  tokenStatus,
  type ChannelDraft,
} from "./channelsConfig";
import { RestartGateway } from "./RestartGateway";

// ChannelsPage shows the channels' state, configures Telegram and WeChat
// through GET and PATCH /ui/channels/config (saved tokens are never sent
// to the page: it shows whether one is set and a short hint) and handles
// pairing requests.
export function ChannelsPage() {
  const [status, setStatus] = useState<Status | null>(null);
  const [pairing, setPairing] = useState<PairingView | null>(null);
  const [configs, setConfigs] = useState<ChannelConfig[] | null>(null);
  const [configError, setConfigError] = useState("");
  const [error, setError] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    setError("");
    setConfigError("");
    const configsLoad = listChannelConfigs()
      .then(setConfigs)
      .catch((err: Error) => {
        setConfigs(null);
        setConfigError(err.message);
      });
    try {
      const [st, p] = await Promise.all([getStatus(), getPairing()]);
      setStatus(st);
      setPairing(p);
    } catch (err) {
      setError((err as Error).message);
    }
    await configsLoad;
  }, []);

  const savedConfig = (c: ChannelConfig) => setConfigs((list) => list && list.map((q) => (q.name === c.name ? c : q)));
  const pending = (configs ?? []).filter((c) => c.restart_required).map((c) => c.name);

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

      <h3>Configure</h3>
      {configError && <p className="page-error">Cannot load the channel settings ({configError}).</p>}
      {!configError && !configs && <p className="page-empty">Loading…</p>}
      {pending.length > 0 && (
        <p className="page-note">
          Restart the gateway to apply the saved settings of {pending.join(", ")}. Channels are built when the gateway
          starts.
        </p>
      )}
      {pending.length > 0 && <RestartGateway />}
      {configs?.map((c) => <ChannelConfigForm key={c.name} config={c} onSaved={savedConfig} />)}
      {configs && (
        <p className="page-empty">
          Saved to <code>channels.&lt;name&gt;</code> in config.json. <code>NANOBOT_GO_TELEGRAM_TOKEN</code> and{" "}
          <code>NANOBOT_GO_WEIXIN_TOKEN</code> in the environment win over the file. The WebUI (websocket) channel&apos;s
          secrets are not editable here.
        </p>
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

const channelTitles: Record<string, string> = { telegram: "Telegram", weixin: "WeChat" };

function ChannelConfigForm({ config: c, onSaved }: { config: ChannelConfig; onSaved: (c: ChannelConfig) => void }) {
  const [draft, setDraft] = useState<ChannelDraft>(() => channelDraft(c));
  const [note, setNote] = useState<{ kind: "" | "ok" | "error"; text: string }>({ kind: "", text: "" });
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);

  useEffect(() => setDraft(channelDraft(c)), [c]);

  const set = <K extends keyof ChannelDraft>(k: K, v: ChannelDraft[K]) => setDraft((d) => ({ ...d, [k]: v }));

  const submit = async (ev: FormEvent) => {
    ev.preventDefault();
    if (saving) return;
    const bad = checkChannel(c, draft);
    if (bad) {
      setNote({ kind: "error", text: "Not saved: " + bad + "." });
      return;
    }
    const patch = diffChannel(c, draft);
    if (!Object.keys(patch).length) {
      setNote({ kind: "", text: "Nothing changed." });
      return;
    }
    setSaving(true);
    setNote({ kind: "", text: "Saving…" });
    try {
      onSaved(await saveChannelConfig(c.name, patch));
      setNote({ kind: "ok", text: "Saved to config.json." });
    } catch (err) {
      setNote({ kind: "error", text: "Not saved: " + (err as Error).message });
    } finally {
      setSaving(false);
    }
  };

  const test = async () => {
    if (testing || saving) return;
    const token = draft.token.trim();
    const bad = checkToken(c.name, token);
    if (bad) {
      setNote({ kind: "error", text: "Not tested: " + bad + "." });
      return;
    }
    setTesting(true);
    setNote({ kind: "", text: "Testing…" });
    try {
      setNote(testNote(await testChannelConfig(c.name, token || undefined)));
    } catch (err) {
      setNote({ kind: "error", text: "Not tested: " + (err as Error).message });
    } finally {
      setTesting(false);
    }
  };

  const id = "channel-" + c.name;
  const dirty = Object.keys(diffChannel(c, draft)).length > 0;
  const tokenLabel = c.name === "telegram" ? "New bot token" : "New token";
  return (
    <form className="cron-form" autoComplete="off" onSubmit={(e) => void submit(e)} aria-labelledby={id + "-heading"}>
      <h3 id={id + "-heading"}>
        {channelTitles[c.name] ?? c.name} <small>({channelState(c)})</small>{" "}
        {c.restart_required && <span className="badge">restart required</span>}
      </h3>
      <p className="page-empty">Token: {tokenStatus(c)}</p>
      <fieldset className="cron-fields" disabled={saving}>
        <label htmlFor={id + "-enabled"}>
          <input
            id={id + "-enabled"}
            type="checkbox"
            checked={draft.enabled}
            onChange={(e) => set("enabled", e.target.checked)}
          />{" "}
          Enabled
        </label>
        <label htmlFor={id + "-token"}>
          {tokenLabel}
          <input
            id={id + "-token"}
            type="password"
            autoComplete="new-password"
            spellCheck={false}
            maxLength={4096}
            placeholder={
              c.hasToken
                ? "Leave empty to keep the saved token"
                : c.name === "telegram"
                  ? "123456:ABC… from @BotFather"
                  : "Usually empty: log in with the CLI"
            }
            disabled={draft.clearToken}
            value={draft.token}
            onChange={(e) => set("token", e.target.value)}
          />
        </label>
        <label htmlFor={id + "-allow"}>
          Allow from
          <input
            id={id + "-allow"}
            type="text"
            spellCheck={false}
            placeholder="Empty: only paired senders; * allows everyone"
            value={draft.allowFrom}
            onChange={(e) => set("allowFrom", e.target.value)}
          />
        </label>
        {c.name === "telegram" && (
          <>
            <label htmlFor={id + "-group"}>
              Group messages
              <select id={id + "-group"} value={draft.groupPolicy} onChange={(e) => set("groupPolicy", e.target.value)}>
                <option value="">Default (mentions and replies)</option>
                <option value="mention">Mentions and replies</option>
                <option value="open">Every message</option>
              </select>
            </label>
            <label htmlFor={id + "-reply"}>
              <input
                id={id + "-reply"}
                type="checkbox"
                checked={draft.replyToMessage}
                onChange={(e) => set("replyToMessage", e.target.checked)}
              />{" "}
              Answer as a reply to the message
            </label>
            <label htmlFor={id + "-base"}>
              API base
              <input
                id={id + "-base"}
                type="text"
                inputMode="url"
                spellCheck={false}
                maxLength={2048}
                placeholder={c.defaultBase}
                value={draft.apiBase}
                onChange={(e) => set("apiBase", e.target.value)}
              />
            </label>
          </>
        )}
        {c.name === "weixin" && (
          <>
            <label htmlFor={id + "-base"}>
              Base URL
              <input
                id={id + "-base"}
                type="text"
                inputMode="url"
                spellCheck={false}
                maxLength={2048}
                placeholder={c.defaultBase}
                value={draft.baseUrl}
                onChange={(e) => set("baseUrl", e.target.value)}
              />
            </label>
            <label htmlFor={id + "-route"}>
              Route tag
              <input
                id={id + "-route"}
                type="text"
                spellCheck={false}
                maxLength={256}
                placeholder="Optional"
                value={draft.routeTag}
                onChange={(e) => set("routeTag", e.target.value)}
              />
            </label>
          </>
        )}
        <label htmlFor={id + "-poll"}>
          Poll timeout (seconds)
          <input
            id={id + "-poll"}
            type="text"
            inputMode="numeric"
            placeholder="Default"
            value={draft.pollTimeout}
            onChange={(e) => set("pollTimeout", e.target.value)}
          />
        </label>
      </fieldset>
      {c.name === "weixin" && (
        <p className="page-empty">
          WeChat logs in by QR code: run <code>nanobot channels login weixin</code> in a terminal. The login is saved
          next to config.json and used when no token is set here.
        </p>
      )}
      {c.hasProxy && <p className="page-empty">A proxy is configured in config.json (not shown).</p>}
      <div className="cron-form-actions">
        <button
          type="button"
          disabled={saving || !c.hasToken}
          aria-pressed={draft.clearToken}
          onClick={() => setDraft((d) => ({ ...d, token: "", clearToken: !d.clearToken }))}
        >
          {draft.clearToken ? "Keep token" : "Clear token"}
        </button>
        {c.testSupported && (
          <button type="button" disabled={saving || testing || !canTest(c, draft)} onClick={() => void test()}>
            {testing ? "Testing…" : "Test"}
          </button>
        )}
        <button type="button" disabled={saving || !dirty} onClick={() => setDraft(channelDraft(c))}>
          Reset
        </button>
        <button type="submit" disabled={saving || !dirty}>
          {c.present ? "Save" : "Add to config"}
        </button>
      </div>
      {note.text && (
        <p className={note.kind === "error" ? "page-error" : "page-note"} role="status">
          {note.text}
        </p>
      )}
    </form>
  );
}
