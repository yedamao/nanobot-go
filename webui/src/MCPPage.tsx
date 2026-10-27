import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import {
  cancelMCPOAuth,
  completeMCPOAuth,
  deleteMCPServer,
  listMCP,
  mcpOAuthStatus,
  putMCPServer,
  setMCPServerEnabled,
  startMCPOAuth,
  testMCPServer,
  type MCPList,
  type MCPOAuthFlow,
  type MCPServer,
} from "./api";
import {
  buildMCPInput,
  canAuthorizeMCP,
  canTestMCP,
  emptyMCPForm,
  mcpBadges,
  mcpOAuthNote,
  mcpTarget,
  mcpTestNote,
  oauthFlowActive,
  type MCPForm,
} from "./mcp";
import { RestartGateway } from "./RestartGateway";
import { Fill, useT, type MessageKey } from "./i18n";

export function MCPPage() {
  const t = useT();
  const [list, setList] = useState<MCPList | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [note, setNote] = useState("");
  const [form, setForm] = useState<MCPForm>(emptyMCPForm);
  // tests holds each server's last test note; testing names the one
  // being tested.
  const [tests, setTests] = useState<Record<string, { kind: "" | "ok" | "error"; text: string }>>({});
  const [testing, setTesting] = useState<string | null>(null);
  // flows holds each server's latest sign-in; pastes the callback URLs
  // typed for manual completion.
  const [flows, setFlows] = useState<Record<string, MCPOAuthFlow>>({});
  const [flowErrors, setFlowErrors] = useState<Record<string, string>>({});
  const [pastes, setPastes] = useState<Record<string, string>>({});
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const load = useCallback(async () => {
    setError("");
    try {
      setList(await listMCP());
    } catch (err) {
      setList(null);
      setError((err as Error).message);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // act runs one change, then reloads the list.
  const act = async (key: string, done: string, change: () => Promise<unknown>): Promise<boolean> => {
    setBusy(key);
    setNote("");
    try {
      await change();
      setNote(done);
      await load();
      return true;
    } catch (err) {
      setNote(t("mcp.failed", { message: (err as Error).message }));
      return false;
    } finally {
      setBusy(null);
    }
  };

  const toggle = (s: MCPServer) =>
    act(s.name, t(s.enabled ? "mcp.disabledNote" : "mcp.enabledNote", { name: s.name }), () =>
      setMCPServerEnabled(s.name, !s.enabled),
    );
  const test = async (s: MCPServer) => {
    if (testing !== null || busy !== null) return;
    setTesting(s.name);
    setTests((prev) => ({ ...prev, [s.name]: { kind: "", text: t("common.testing") } }));
    let note: { kind: "" | "ok" | "error"; text: string };
    try {
      note = mcpTestNote(await testMCPServer(s.name), t);
    } catch (err) {
      note = { kind: "error", text: t("common.notTested", { message: (err as Error).message }) };
    }
    setTests((prev) => ({ ...prev, [s.name]: note }));
    setTesting(null);
  };
  const setFlow = (name: string, f: MCPOAuthFlow) => setFlows((prev) => ({ ...prev, [name]: f }));
  const setFlowError = (name: string, msg: string) => setFlowErrors((prev) => ({ ...prev, [name]: msg }));

  // poll follows a sign-in until it ends, then reloads the list.
  const poll = async (name: string, flow: MCPOAuthFlow) => {
    let f = flow;
    while (mounted.current && oauthFlowActive(f)) {
      await new Promise((r) => setTimeout(r, 1500));
      if (!mounted.current) return;
      try {
        f = await mcpOAuthStatus(name, f.flow_id);
      } catch (err) {
        setFlowError(name, (err as Error).message);
        return;
      }
      setFlow(name, f);
    }
    if (mounted.current) await load();
  };

  const authorize = async (s: MCPServer, reset: boolean) => {
    setFlowError(s.name, "");
    // Open the window now, while the click allows it; the sign-in page
    // is loaded into it once the gateway has its URL.
    const win = window.open("about:blank", "_blank");
    let f: MCPOAuthFlow;
    try {
      f = await startMCPOAuth(s.name, reset);
    } catch (err) {
      win?.close();
      setFlowError(s.name, (err as Error).message);
      return;
    }
    setFlow(s.name, f);
    if (f.authorization_url && win) {
      win.opener = null;
      win.location.href = f.authorization_url;
    } else {
      win?.close();
    }
    void poll(s.name, f);
  };

  const complete = async (s: MCPServer) => {
    const f = flows[s.name];
    if (!f) return;
    setFlowError(s.name, "");
    try {
      setFlow(s.name, await completeMCPOAuth(s.name, f.flow_id, (pastes[s.name] ?? "").trim()));
      setPastes((prev) => ({ ...prev, [s.name]: "" }));
    } catch (err) {
      setFlowError(s.name, (err as Error).message);
    }
  };

  const cancelFlow = async (s: MCPServer) => {
    const f = flows[s.name];
    if (!f) return;
    try {
      setFlow(s.name, await cancelMCPOAuth(s.name, f.flow_id));
    } catch (err) {
      setFlowError(s.name, (err as Error).message);
    }
  };

  const remove = (s: MCPServer) => {
    if (!confirm(t("mcp.removeConfirm", { name: s.name }))) return;
    void act(s.name, t("mcp.removed", { name: s.name }), () => deleteMCPServer(s.name));
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const built = buildMCPInput(form);
    if ("error" in built) {
      setNote(t("mcp.cannotAdd", { message: t(built.error.key, built.error.vars) }));
      return;
    }
    if (list?.servers.some((s) => s.name === built.name && !s.removed)) {
      if (!confirm(t("mcp.replaceConfirm", { name: built.name }))) return;
    }
    if (await act("+add", t("mcp.saved", { name: built.name }), () => putMCPServer(built.name, built.input))) {
      setForm(emptyMCPForm);
    }
  };

  const set = (patch: Partial<MCPForm>) => setForm({ ...form, ...patch });
  // stateLabel names a live connection state; unknown ones show as sent.
  const stateLabel = (state: string) => {
    const key = ("mcp.states." + state) as MessageKey;
    const label = t(key);
    return label === key ? state.replace("_", " ") : label;
  };

  return (
    <section className="page" aria-labelledby="mcp-heading">
      <header className="page-header">
        <h2 id="mcp-heading">{t("mcp.heading")}</h2>
        <button type="button" onClick={() => void load()} disabled={busy !== null}>
          {t("common.refresh")}
        </button>
      </header>
      {error && <p className="page-error">{t("mcp.loadError", { error })}</p>}
      {note && <p className="page-note">{note}</p>}
      {list && list.restart_required.length > 0 && (
        <p className="page-note">{t("mcp.pending", { names: list.restart_required.join(", ") })}</p>
      )}
      {list && list.restart_required.length > 0 && <RestartGateway />}
      {!error && list === null && <p className="page-empty">{t("common.loading")}</p>}
      {list &&
        (list.servers.length === 0 ? (
          <p className="page-empty">
            <Fill text={t("mcp.empty")} vars={{ path: <code>tools.mcpServers</code> }} />
          </p>
        ) : (
          <ul className="page-list skills-list">
            {list.servers.map((s) => (
              <li key={s.name} data-disabled={s.enabled ? undefined : ""}>
                <div className="skill-head">
                  <strong>{s.name}</strong>
                  {mcpBadges(s, t).map((b) => (
                    <span key={b} className="badge">
                      {b}
                    </span>
                  ))}
                  {list.live && (
                    <span className="badge" data-state={s.state}>
                      {stateLabel(s.state)}
                    </span>
                  )}
                  {!s.removed && (
                    <span className="page-actions">
                      <button
                        type="button"
                        disabled={busy !== null || testing !== null || !canTestMCP(s)}
                        title={s.enabled ? undefined : t("mcp.testDisabled")}
                        onClick={() => void test(s)}
                      >
                        {t(testing === s.name ? "common.testing" : "common.test")}
                      </button>
                      {canAuthorizeMCP(s) && (
                        <button
                          type="button"
                          disabled={busy !== null || (!!flows[s.name] && oauthFlowActive(flows[s.name]))}
                          onClick={() => void authorize(s, s.oauth === "authorized")}
                        >
                          {t(s.oauth === "authorized" ? "mcp.oauth.signInAgain" : "mcp.oauth.signIn")}
                        </button>
                      )}
                      <button type="button" disabled={busy !== null || testing === s.name} onClick={() => void toggle(s)}>
                        {t(s.enabled ? "common.disable" : "common.enable")}
                      </button>
                      <button type="button" disabled={busy !== null || testing === s.name} onClick={() => remove(s)}>
                        {t("common.remove")}
                      </button>
                    </span>
                  )}
                </div>
                <code className="skill-path">{mcpTarget(s)}</code>
                {!!(s.env_keys?.length || s.header_keys?.length || s.enabled_tools || s.tool_timeout || s.oauth) && (
                  <p>
                    {s.oauth
                      ? t("mcp.oauth.status", {
                          state: t(s.oauth === "authorized" ? "mcp.oauth.authorizedState" : "mcp.oauth.requiredState"),
                        })
                      : ""}
                    {s.env_keys?.length ? t("mcp.env", { keys: s.env_keys.join(", ") }) : ""}
                    {s.header_keys?.length ? t("mcp.headers", { keys: s.header_keys.join(", ") }) : ""}
                    {s.enabled_tools ? t("mcp.toolsAllowed", { tools: s.enabled_tools.join(", ") || t("common.none") }) : ""}
                    {s.tool_timeout ? t("mcp.timeout", { seconds: s.tool_timeout }) : ""}
                  </p>
                )}
                {s.error && <p className="page-error">{s.error}</p>}
                {s.tools && s.tools.length > 0 && <p>{t("mcp.tools", { tools: s.tools.join(", ") })}</p>}
                {!s.removed && flows[s.name] && <OAuthPanel
                  flow={flows[s.name]}
                  paste={pastes[s.name] ?? ""}
                  onPaste={(v) => setPastes((prev) => ({ ...prev, [s.name]: v }))}
                  onComplete={() => void complete(s)}
                  onCancel={() => void cancelFlow(s)}
                />}
                {!s.removed && flowErrors[s.name] && (
                  <p className="page-error" role="status">
                    {t("mcp.oauth.failed", { message: flowErrors[s.name] })}
                  </p>
                )}
                {!s.removed && tests[s.name] && (
                  <p className={tests[s.name].kind === "error" ? "page-error" : "page-note"} role="status">
                    {tests[s.name].text}
                  </p>
                )}
              </li>
            ))}
          </ul>
        ))}

      <form className="cron-form" onSubmit={(e) => void submit(e)} autoComplete="off">
        <h3>{t("mcp.add")}</h3>
        <fieldset className="cron-fields">
          <label htmlFor="mcp-name">
            {t("mcp.fields.name")}
            <input id="mcp-name" type="text" required value={form.name} placeholder={t("mcp.fields.namePlaceholder")} onChange={(e) => set({ name: e.target.value })} />
          </label>
          <label htmlFor="mcp-kind">
            {t("mcp.fields.transport")}
            <select id="mcp-kind" value={form.kind} onChange={(e) => set({ kind: e.target.value as MCPForm["kind"] })}>
              <option value="stdio">{t("mcp.fields.stdio")}</option>
              <option value="http">{t("mcp.fields.http")}</option>
            </select>
          </label>
          {form.kind === "stdio" ? (
            <>
              <label htmlFor="mcp-command">
                {t("mcp.fields.command")}
                <input id="mcp-command" type="text" value={form.command} placeholder="npx" onChange={(e) => set({ command: e.target.value })} />
              </label>
              <label htmlFor="mcp-args">
                {t("mcp.fields.args")}
                <textarea id="mcp-args" rows={3} value={form.args} placeholder={t("mcp.fields.argsPlaceholder")} onChange={(e) => set({ args: e.target.value })} />
              </label>
              <label htmlFor="mcp-env">
                {t("mcp.fields.env")}
                <textarea id="mcp-env" rows={2} value={form.env} placeholder={t("mcp.fields.envPlaceholder")} onChange={(e) => set({ env: e.target.value })} />
              </label>
            </>
          ) : (
            <>
              <label htmlFor="mcp-url">
                {t("mcp.fields.url")}
                <input id="mcp-url" type="text" value={form.url} placeholder="https://host/mcp" onChange={(e) => set({ url: e.target.value })} />
              </label>
              <label htmlFor="mcp-type">
                {t("mcp.fields.type")}
                <select id="mcp-type" value={form.type} onChange={(e) => set({ type: e.target.value as MCPForm["type"] })}>
                  <option value="">{t("mcp.fields.typeAuto")}</option>
                  <option value="streamableHttp">streamableHttp</option>
                  <option value="sse">sse</option>
                </select>
              </label>
              <label htmlFor="mcp-auth">
                {t("mcp.fields.auth")}
                <select id="mcp-auth" value={form.auth} onChange={(e) => set({ auth: e.target.value as MCPForm["auth"] })}>
                  <option value="headers">{t("mcp.fields.authHeaders")}</option>
                  <option value="oauth">{t("mcp.fields.authOAuth")}</option>
                </select>
              </label>
              {form.auth === "headers" ? (
                <label htmlFor="mcp-headers">
                  {t("mcp.fields.headers")}
                  <textarea id="mcp-headers" rows={2} value={form.headers} placeholder="Authorization: Bearer ${TOKEN}" onChange={(e) => set({ headers: e.target.value })} />
                </label>
              ) : (
                <p className="page-empty">{t("mcp.oauth.help")}</p>
              )}
            </>
          )}
          <label htmlFor="mcp-tools">
            {t("mcp.fields.tools")}
            <input id="mcp-tools" type="text" value={form.enabledTools} placeholder={t("mcp.fields.toolsPlaceholder")} onChange={(e) => set({ enabledTools: e.target.value })} />
          </label>
          <label htmlFor="mcp-timeout">
            {t("mcp.fields.timeout")}
            <input id="mcp-timeout" type="number" min={0} step={1} value={form.toolTimeout} placeholder="30" onChange={(e) => set({ toolTimeout: e.target.value })} />
          </label>
          <label className="check">
            <input type="checkbox" checked={form.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
            {t("common.enabled")}
          </label>
        </fieldset>
        <div className="cron-form-actions">
          <button type="submit" disabled={busy !== null}>
            {t("common.save")}
          </button>
        </div>
      </form>
      <p className="page-empty">
        <Fill text={t("mcp.help")} vars={{ ref: <code>${"{VAR}"}</code> }} />
      </p>
    </section>
  );
}

// OAuthPanel shows a sign-in in progress: the sign-in link, the paste
// box for a callback the gateway cannot receive, and the outcome.
function OAuthPanel(props: {
  flow: MCPOAuthFlow;
  paste: string;
  onPaste: (v: string) => void;
  onComplete: () => void;
  onCancel: () => void;
}) {
  const t = useT();
  const { flow } = props;
  const note = mcpOAuthNote(flow, t);
  const waiting = flow.status === "authorization_required";
  return (
    <div className="mcp-oauth" role="status">
      <p className={note.kind === "error" ? "page-error" : "page-note"}>{note.text}</p>
      {waiting && flow.authorization_url && (
        <p>
          <a href={flow.authorization_url} target="_blank" rel="noopener noreferrer">
            {t("mcp.oauth.open")}
          </a>
        </p>
      )}
      {waiting && flow.completion_input === "callback_url" && (
        <>
          <p className="page-empty">
            <Fill text={t("mcp.oauth.pasteHelp")} vars={{ uri: <code>{flow.redirect_uri ?? ""}</code> }} />
          </p>
          <label>
            {t("mcp.oauth.pasteLabel")}
            <input type="url" value={props.paste} onChange={(e) => props.onPaste(e.target.value)} />
          </label>
          <button type="button" disabled={!props.paste.trim()} onClick={props.onComplete}>
            {t("mcp.oauth.complete")}
          </button>
        </>
      )}
      {oauthFlowActive(flow) && (
        <button type="button" onClick={props.onCancel}>
          {t("mcp.oauth.cancel")}
        </button>
      )}
    </div>
  );
}
