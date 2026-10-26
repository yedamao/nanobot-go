import { useCallback, useEffect, useState, type FormEvent } from "react";
import {
  approvePairing,
  clearChannelLogin,
  denyPairing,
  getPairing,
  getStatus,
  listChannelConfigs,
  revokePairing,
  saveChannelConfig,
  testChannelConfig,
  type ChannelConfig,
  type PairingView,
  type WeixinLogin,
  type Status,
} from "./api";
import {
  canClearLogin,
  canTest,
  channelDraft,
  clearLoginPrompt,
  channelState,
  checkChannel,
  checkToken,
  diffChannel,
  forceLoginCommand,
  loginDetails,
  loginStatus,
  testNote,
  tokenStatus,
  weixinLogin,
  withLogin,
  type ChannelDraft,
} from "./channelsConfig";
import { RestartGateway } from "./RestartGateway";
import { Fill, useFormatDate, useT, type MessageKey } from "./i18n";

// ChannelsPage shows the channels' state, configures Telegram and WeChat
// through GET and PATCH /ui/channels/config (saved tokens are never sent
// to the page: it shows whether one is set and a short hint) and handles
// pairing requests.
export function ChannelsPage() {
  const t = useT();
  const formatDate = useFormatDate();
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

  // act runs a pairing change; done is its note, action names it if it fails.
  const act = async (done: string, action: MessageKey, fn: () => Promise<void>) => {
    setBusy(true);
    setNote("");
    try {
      await fn();
      setNote(done);
      await load();
    } catch (err) {
      setNote(t("channels.actionFailed", { action: t(action), message: (err as Error).message }));
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
        <h2 id="channels-heading">{t("channels.heading")}</h2>
        <button type="button" onClick={() => void load()} disabled={busy}>
          {t("common.refresh")}
        </button>
      </header>
      {error && <p className="page-error">{t("channels.loadError", { error })}</p>}
      {note && <p className="page-note">{note}</p>}

      <h3>{t("channels.configured")}</h3>
      {!status && !error && <p className="page-empty">{t("common.loading")}</p>}
      {status && (status.channels?.length ?? 0) === 0 && <p className="page-empty">{t("channels.noneConfigured")}</p>}
      {status && (status.channels?.length ?? 0) > 0 && (
        <ul className="page-list">
          {(status.channels ?? []).map((c) => (
            <li key={c.name}>
              <strong>{c.name}</strong> —{" "}
              {t(c.running ? "common.running" : c.enabled ? "common.enabledNotRunning" : "common.disabled")}
            </li>
          ))}
        </ul>
      )}

      <h3>{t("channels.configure")}</h3>
      {configError && <p className="page-error">{t("channels.configLoadError", { error: configError })}</p>}
      {!configError && !configs && <p className="page-empty">{t("common.loading")}</p>}
      {pending.length > 0 && <p className="page-note">{t("channels.pending", { names: pending.join(", ") })}</p>}
      {pending.length > 0 && <RestartGateway />}
      {configs?.map((c) => <ChannelConfigForm key={c.name} config={c} onSaved={savedConfig} />)}
      {configs && (
        <p className="page-empty">
          <Fill text={t("channels.savedTo")} vars={{ path: <code>channels.&lt;name&gt;</code> }} />{" "}
          <Fill
            text={t("channels.envWins")}
            vars={{ telegram: <code>NANOBOT_GO_TELEGRAM_TOKEN</code>, weixin: <code>NANOBOT_GO_WEIXIN_TOKEN</code> }}
          />{" "}
          {t("channels.websocketNote")}
        </p>
      )}

      <h3>{t("channels.pendingPairing")}</h3>
      {pairing && pairing.pending.length === 0 && <p className="page-empty">{t("channels.noPending")}</p>}
      {pairing && pairing.pending.length > 0 && (
        <table className="page-table">
          <thead>
            <tr>
              <th>{t("channels.columns.code")}</th>
              <th>{t("channels.columns.channel")}</th>
              <th>{t("channels.columns.sender")}</th>
              <th>{t("channels.columns.expires")}</th>
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
                <td>{formatDate(p.expires_at)}</td>
                <td className="page-actions">
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => void act(t("channels.approved", { code: p.code }), "channels.approve", () => approvePairing(p.code))}
                  >
                    {t("channels.approve")}
                  </button>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => void act(t("channels.denied", { code: p.code }), "channels.deny", () => denyPairing(p.code))}
                  >
                    {t("channels.deny")}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h3>{t("channels.approvedSenders")}</h3>
      {approvedRows.length === 0 && <p className="page-empty">{t("channels.noneYet")}</p>}
      {approvedRows.length > 0 && (
        <table className="page-table">
          <thead>
            <tr>
              <th>{t("channels.columns.channel")}</th>
              <th>{t("channels.columns.sender")}</th>
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
                      void act(t("channels.revoked", { sender: r.sender }), "channels.revoke", () =>
                        revokePairing(r.channel, r.sender),
                      )
                    }
                  >
                    {t("channels.revoke")}
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
  const t = useT();
  const [draft, setDraft] = useState<ChannelDraft>(() => channelDraft(c));
  const [note, setNote] = useState<{ kind: "" | "ok" | "error"; text: string }>({ kind: "", text: "" });
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);

  // The draft resets when the saved settings change, not when only the
  // login or restart state does, so unsaved edits survive clearing the
  // login.
  const saved = JSON.stringify(channelDraft(c));
  useEffect(() => setDraft(channelDraft(c)), [saved]);

  const set = <K extends keyof ChannelDraft>(k: K, v: ChannelDraft[K]) => setDraft((d) => ({ ...d, [k]: v }));

  const submit = async (ev: FormEvent) => {
    ev.preventDefault();
    if (saving) return;
    const bad = checkChannel(c, draft);
    if (bad) {
      setNote({ kind: "error", text: t("common.notSavedInvalid", { message: bad }) });
      return;
    }
    const patch = diffChannel(c, draft);
    if (!Object.keys(patch).length) {
      setNote({ kind: "", text: t("common.nothingChanged") });
      return;
    }
    setSaving(true);
    setNote({ kind: "", text: t("common.saving") });
    try {
      onSaved(await saveChannelConfig(c.name, patch));
      setNote({ kind: "ok", text: t("common.savedToConfig") });
    } catch (err) {
      setNote({ kind: "error", text: t("common.notSaved", { message: (err as Error).message }) });
    } finally {
      setSaving(false);
    }
  };

  const test = async () => {
    if (testing || saving) return;
    const token = draft.token.trim();
    const bad = checkToken(c.name, token);
    if (bad) {
      setNote({ kind: "error", text: t("common.notTestedInvalid", { message: bad }) });
      return;
    }
    setTesting(true);
    setNote({ kind: "", text: t("common.testing") });
    try {
      setNote(testNote(await testChannelConfig(c.name, token || undefined), t));
    } catch (err) {
      setNote({ kind: "error", text: t("common.notTested", { message: (err as Error).message }) });
    } finally {
      setTesting(false);
    }
  };

  const id = "channel-" + c.name;
  const dirty = Object.keys(diffChannel(c, draft)).length > 0;
  const tokenLabel = t(c.name === "telegram" ? "channels.newBotToken" : "channels.newToken");
  return (
    <form className="cron-form" autoComplete="off" onSubmit={(e) => void submit(e)} aria-labelledby={id + "-heading"}>
      <h3 id={id + "-heading"}>
        {channelTitles[c.name] ?? c.name} <small>({channelState(c, t)})</small>{" "}
        {c.restart_required && <span className="badge">{t("common.restartRequired")}</span>}
      </h3>
      <p className="page-empty">{t("channels.tokenLabel", { status: tokenStatus(c, t) })}</p>
      <fieldset className="cron-fields" disabled={saving}>
        <label htmlFor={id + "-enabled"}>
          <input
            id={id + "-enabled"}
            type="checkbox"
            checked={draft.enabled}
            onChange={(e) => set("enabled", e.target.checked)}
          />{" "}
          {t("common.enabled")}
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
                ? t("channels.keepTokenPlaceholder")
                : c.name === "telegram"
                  ? t("channels.telegramTokenPlaceholder")
                  : t("channels.weixinTokenPlaceholder")
            }
            disabled={draft.clearToken}
            value={draft.token}
            onChange={(e) => set("token", e.target.value)}
          />
        </label>
        <label htmlFor={id + "-allow"}>
          {t("channels.allowFrom")}
          <input
            id={id + "-allow"}
            type="text"
            spellCheck={false}
            placeholder={t("channels.allowFromPlaceholder")}
            value={draft.allowFrom}
            onChange={(e) => set("allowFrom", e.target.value)}
          />
        </label>
        {c.name === "telegram" && (
          <>
            <label htmlFor={id + "-group"}>
              {t("channels.groupMessages")}
              <select id={id + "-group"} value={draft.groupPolicy} onChange={(e) => set("groupPolicy", e.target.value)}>
                <option value="">{t("channels.groupDefault")}</option>
                <option value="mention">{t("channels.groupMention")}</option>
                <option value="open">{t("channels.groupOpen")}</option>
              </select>
            </label>
            <label htmlFor={id + "-reply"}>
              <input
                id={id + "-reply"}
                type="checkbox"
                checked={draft.replyToMessage}
                onChange={(e) => set("replyToMessage", e.target.checked)}
              />{" "}
              {t("channels.replyToMessage")}
            </label>
            <label htmlFor={id + "-base"}>
              {t("common.apiBase")}
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
              {t("channels.baseUrl")}
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
              {t("channels.routeTag")}
              <input
                id={id + "-route"}
                type="text"
                spellCheck={false}
                maxLength={256}
                placeholder={t("channels.optional")}
                value={draft.routeTag}
                onChange={(e) => set("routeTag", e.target.value)}
              />
            </label>
          </>
        )}
        <label htmlFor={id + "-poll"}>
          {t("channels.pollTimeout")}
          <input
            id={id + "-poll"}
            type="text"
            inputMode="numeric"
            placeholder={t("channels.default")}
            value={draft.pollTimeout}
            onChange={(e) => set("pollTimeout", e.target.value)}
          />
        </label>
      </fieldset>
      {c.name === "weixin" && c.login && (
        <WeixinLoginStatus login={c.login} running={c.running} onCleared={(v) => onSaved(withLogin(c, v))} />
      )}
      {c.name === "weixin" && !c.login && (
        <p className="page-empty">
          <Fill text={t("channels.weixinNoLogin")} vars={{ command: <code>nanobot channels login weixin</code> }} />
        </p>
      )}
      {c.hasProxy && <p className="page-empty">{t("channels.proxy")}</p>}
      <div className="cron-form-actions">
        <button
          type="button"
          disabled={saving || !c.hasToken}
          aria-pressed={draft.clearToken}
          onClick={() => setDraft((d) => ({ ...d, token: "", clearToken: !d.clearToken }))}
        >
          {t(draft.clearToken ? "channels.keepToken" : "channels.clearToken")}
        </button>
        {c.testSupported && (
          <button type="button" disabled={saving || testing || !canTest(c, draft)} onClick={() => void test()}>
            {t(testing ? "common.testing" : "common.test")}
          </button>
        )}
        <button type="button" disabled={saving || !dirty} onClick={() => setDraft(channelDraft(c))}>
          {t("common.reset")}
        </button>
        <button type="submit" disabled={saving || !dirty}>
          {t(c.present ? "common.save" : "channels.addToConfig")}
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

// WeixinLoginStatus shows the saved WeChat login (never its token) and
// the CLI command that logs in: the QR code is shown only in a terminal.
// Refresh re-reads the login and Clear login removes the saved one
// (after a confirm), both without resetting the card's form.
function WeixinLoginStatus({
  login: initial,
  running,
  onCleared,
}: {
  login: WeixinLogin;
  running: boolean;
  onCleared: (v: ChannelConfig) => void;
}) {
  const t = useT();
  const [login, setLogin] = useState(initial);
  const [note, setNote] = useState("");
  const [refreshing, setRefreshing] = useState(false);
  const [clearing, setClearing] = useState(false);

  useEffect(() => setLogin(initial), [initial]);

  const refresh = async () => {
    setRefreshing(true);
    setNote("");
    try {
      const l = weixinLogin(await listChannelConfigs());
      if (l) setLogin(l);
      setNote(t("channels.login.refreshed"));
    } catch (err) {
      setNote(t("channels.login.refreshFailed", { message: (err as Error).message }));
    } finally {
      setRefreshing(false);
    }
  };

  const clear = async () => {
    if (clearing || !confirm(clearLoginPrompt(login, running, t))) return;
    setClearing(true);
    setNote("");
    try {
      const v = await clearChannelLogin("weixin");
      if (v.login) setLogin(v.login);
      onCleared(v);
      setNote(
        t(v.restart_required && running ? "channels.login.clearedRestart" : "channels.login.cleared"),
      );
    } catch (err) {
      setNote(t("channels.login.clearFailed", { message: (err as Error).message }));
    } finally {
      setClearing(false);
    }
  };

  const copy = (cmd: string) => {
    if (!navigator.clipboard) {
      setNote(t("channels.login.copyUnavailable"));
      return;
    }
    navigator.clipboard.writeText(cmd).then(
      () => setNote(t("channels.login.copied")),
      () => setNote(t("channels.login.copyFailed")),
    );
  };

  const status = loginStatus(login, t);
  const details = loginDetails(login, t);
  return (
    <div className="weixin-login" aria-label={t("channels.login.label")}>
      <p className={status.kind === "error" ? "page-error" : "page-note"}>
        <strong>{t("channels.login.status")}</strong>{" "}
        <span className="badge" data-state={status.kind === "ok" ? "connected" : status.kind === "error" ? "failed" : undefined}>
          {status.text}
        </span>
      </p>
      {details.length > 0 && (
        <dl className="weixin-login-details">
          {details.map(([k, v]) => (
            <div key={k}>
              <dt>{k}</dt>
              <dd>
                <code>{v}</code>
              </dd>
            </div>
          ))}
        </dl>
      )}
      <p className="page-empty">{login.loginHint}</p>
      <label htmlFor="weixin-login-cmd">
        {t("channels.login.fromTerminal")}
        <input id="weixin-login-cmd" type="text" readOnly spellCheck={false} value={login.cliCommand} onFocus={(e) => e.target.select()} />
      </label>
      <div className="cron-form-actions">
        <button type="button" onClick={() => copy(login.cliCommand)}>
          {t("channels.login.copy")}
        </button>
        <button type="button" disabled={refreshing} onClick={() => void refresh()}>
          {t(refreshing ? "channels.login.refreshing" : "channels.login.refresh")}
        </button>
        {canClearLogin(login) && (
          <button type="button" disabled={clearing} onClick={() => void clear()}>
            {t(clearing ? "channels.login.clearing" : "channels.login.clear")}
          </button>
        )}
      </div>
      <p className="page-empty">
        {t("channels.login.qrNote")}{" "}
        <Fill
          text={t("channels.login.forceNote")}
          vars={{ flag: <code>--force</code>, command: <code>{forceLoginCommand(login.cliCommand)}</code> }}
        />{" "}
        <Fill text={t("channels.login.savedAt")} vars={{ path: <code>{login.statePath}</code> }} />
      </p>
      {note && (
        <p className="page-note" role="status">
          {note}
        </p>
      )}
    </div>
  );
}
