import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";
import {
  cancelMCPOAuth,
  completeMCPOAuth,
  deleteMCPServer,
  listMCP,
  mcpOAuthStatus,
  putMCPServer,
  reloadMCP,
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
  mcpDoneNote,
  mcpOAuthNote,
  mcpTarget,
  mcpTestNote,
  oauthFlowActive,
  type MCPForm,
} from "./mcp";
import { Icon } from "./Icon";
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
  const [adding, setAdding] = useState(false);
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
      setNote(mcpDoneNote(done, await change(), t));
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
      setAdding(false);
    }
  };

  const set = (patch: Partial<MCPForm>) => setForm({ ...form, ...patch });
  // stateLabel names a live connection state; unknown ones show as sent.
  const stateLabel = (state: string) => {
    const key = ("mcp.states." + state) as MessageKey;
    const label = t(key);
    return label === key ? state.replace("_", " ") : label;
  };

  const reloadAll = () => void act("+reload", "", () => reloadMCP().then((reload) => ({ reload })));
  const servers = list?.servers ?? [];
  const connected = servers.filter((s) => s.state === "connected").length;
  const toolCount = servers.reduce((n, s) => n + (s.tools?.length ?? 0), 0);
  const showForm = adding || (list !== null && servers.length === 0);

  return (
    <section className="page" aria-labelledby="mcp-heading">
      <header className="page-header">
        <div className="page-title">
          <h2 id="mcp-heading">{t("mcp.heading")}</h2>
          <p>
            <Fill text={t("mcp.blurb")} vars={{ path: <code>tools.mcpServers</code> }} />
          </p>
        </div>
        <div className="actions">
          <button type="button" className="btn ghost" onClick={() => void load()} disabled={busy !== null}>
            <Icon name="refresh" />
            {t("common.refresh")}
          </button>
          {list?.live && (
            <button type="button" className="btn" onClick={reloadAll} disabled={busy !== null}>
              <Icon name="rotate" />
              {t("mcp.reload")}
            </button>
          )}
          <button type="button" className="btn primary" aria-expanded={showForm} onClick={() => setAdding(!adding)}>
            <Icon name={adding ? "x" : "plus"} />
            {t(adding ? "common.cancel" : "mcp.addServer")}
          </button>
        </div>
      </header>
      {error && <p className="page-error">{t("mcp.loadError", { error })}</p>}
      {note && <p className="page-note">{note}</p>}
      {list && list.restart_required.length > 0 && (
        <div className="banner" role="status">
          <Icon name="alert" />
          <span>{t("mcp.pending", { names: list.restart_required.join(", ") })}</span>
          {list.live && (
            <button type="button" className="btn sm" onClick={reloadAll} disabled={busy !== null}>
              {t("mcp.reloadNow")}
            </button>
          )}
        </div>
      )}
      {list && list.restart_required.length > 0 && <RestartGateway />}
      {!error && list === null && <p className="page-empty">{t("common.loading")}</p>}
      {list && servers.length > 0 && (
        <div className="stat-row">
          <div className="stat">
            <small>{t("mcp.stats.servers")}</small>
            <strong>{servers.length}</strong>
          </div>
          <div className="stat">
            <small>{t("mcp.stats.connected")}</small>
            <strong className={connected ? "ok" : undefined}>{list.live ? connected : "—"}</strong>
          </div>
          <div className="stat">
            <small>{t("mcp.stats.tools")}</small>
            <strong>{list.live ? toolCount : "—"}</strong>
          </div>
          <div className="stat">
            <small>{t("mcp.stats.pending")}</small>
            <strong className={list.restart_required.length ? "warn" : undefined}>{list.restart_required.length}</strong>
          </div>
        </div>
      )}
      {list && servers.length === 0 && (
        <p className="page-empty">
          <Fill text={t("mcp.empty")} vars={{ path: <code>tools.mcpServers</code> }} />
        </p>
      )}
      {showForm && renderForm()}
      {list && servers.length > 0 && (
        <div className="card-list">
          {servers.map((s) => (
            <article key={s.name} className={"card" + (s.enabled && !s.removed ? "" : " disabled")}>
              <div className="card-head">
                <div className="srv-ico" aria-hidden="true">
                  {s.name.replace(/[^A-Za-z0-9]/g, "").slice(0, 2).toUpperCase() || "?"}
                </div>
                <div className="card-title">
                  <b>
                    <span className="name">{s.name}</span>
                    {list.live && (
                      <span className={"badge " + stateTone(s.state)} data-state={s.state}>
                        {stateTone(s.state) && <span className={"dot " + stateTone(s.state)} />}
                        {stateLabel(s.state)}
                      </span>
                    )}
                    {mcpBadges(s, t).map((b, i) => (
                      <span key={b} className={"badge" + (i === 0 ? " mono" : b === t("common.restartRequired") ? " warn" : "")}>
                        {b}
                      </span>
                    ))}
                  </b>
                  <code className="sub" title={mcpTarget(s)}>
                    {mcpTarget(s)}
                  </code>
                </div>
                {!s.removed && (
                  <div className="card-actions">
                    <button
                      type="button"
                      className="btn sm ghost"
                      disabled={busy !== null || testing !== null || !canTestMCP(s)}
                      title={s.enabled ? undefined : t("mcp.testDisabled")}
                      onClick={() => void test(s)}
                    >
                      {t(testing === s.name ? "common.testing" : "common.test")}
                    </button>
                    {canAuthorizeMCP(s) && (
                      <button
                        type="button"
                        className="btn sm"
                        disabled={busy !== null || (!!flows[s.name] && oauthFlowActive(flows[s.name]))}
                        onClick={() => void authorize(s, s.oauth === "authorized")}
                      >
                        {t(s.oauth === "authorized" ? "mcp.oauth.signInAgain" : "mcp.oauth.signIn")}
                      </button>
                    )}
                    <button
                      type="button"
                      className="icon-btn danger"
                      title={t("common.remove")}
                      aria-label={t("mcp.removeLabel", { name: s.name })}
                      disabled={busy !== null || testing === s.name}
                      onClick={() => remove(s)}
                    >
                      <Icon name="trash" />
                    </button>
                    <label className="switch" title={t(s.enabled ? "common.disable" : "common.enable")}>
                      <input
                        type="checkbox"
                        checked={s.enabled}
                        aria-label={t("mcp.toggleLabel", { name: s.name })}
                        disabled={busy !== null || testing === s.name}
                        onChange={() => void toggle(s)}
                      />
                      <span />
                    </label>
                  </div>
                )}
              </div>
              <MCPCardBody server={s} />
              {s.error && <div className="card-note error">{s.error}</div>}
              {!s.removed && flows[s.name] && (
                <div className="card-note">
                  <OAuthPanel
                    flow={flows[s.name]}
                    paste={pastes[s.name] ?? ""}
                    onPaste={(v) => setPastes((prev) => ({ ...prev, [s.name]: v }))}
                    onComplete={() => void complete(s)}
                    onCancel={() => void cancelFlow(s)}
                  />
                </div>
              )}
              {!s.removed && flowErrors[s.name] && (
                <div className="card-note error" role="status">
                  {t("mcp.oauth.failed", { message: flowErrors[s.name] })}
                </div>
              )}
              {!s.removed && tests[s.name] && (
                <div className={"card-note " + (tests[s.name].kind === "error" ? "error" : tests[s.name].kind === "ok" ? "ok" : "")} role="status">
                  {tests[s.name].text}
                </div>
              )}
            </article>
          ))}
        </div>
      )}
      <p className="page-empty page-foot">
        <Fill text={t("mcp.help")} vars={{ ref: <code>${"{VAR}"}</code> }} />
      </p>
    </section>
  );

  function renderForm() {
    return (
      <form className="cron-form" id="mcp-add" onSubmit={(e) => void submit(e)} autoComplete="off">
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
          <button type="submit" className="btn primary" disabled={busy !== null}>
            {t("common.save")}
          </button>
        </div>
      </form>
    );
  }
}

// stateTone is the badge color of a live connection state.
function stateTone(state: string): "ok" | "warn" | "err" | "" {
  switch (state) {
    case "connected":
      return "ok";
    case "failed":
      return "err";
    case "authorization_required":
    case "pending":
      return "warn";
  }
  return "";
}

const TOOL_CHIPS = 8;

// MCPCardBody shows a server's settings (OAuth, env and header names,
// allowed tools, timeout) and, when connected, its tools as chips.
function MCPCardBody({ server: s }: { server: MCPServer }) {
  const t = useT();
  const [allTools, setAllTools] = useState(false);
  const kv: [string, string][] = [];
  if (s.oauth) kv.push([t("mcp.kv.oauth"), t(s.oauth === "authorized" ? "mcp.oauth.authorizedState" : "mcp.oauth.requiredState")]);
  if (s.env_keys?.length) kv.push([t("mcp.kv.env"), s.env_keys.join(", ")]);
  if (s.header_keys?.length) kv.push([t("mcp.kv.headers"), s.header_keys.join(", ")]);
  if (s.enabled_tools) kv.push([t("mcp.kv.allowed"), s.enabled_tools.join(", ") || t("common.none")]);
  if (s.tool_timeout) kv.push([t("mcp.kv.timeout"), s.tool_timeout + "s"]);
  const tools = s.tools ?? [];
  if (!kv.length && !tools.length) return null;
  const shown = allTools ? tools : tools.slice(0, TOOL_CHIPS);
  return (
    <div className="card-body">
      {kv.length > 0 && (
        <div className="kv">
          {kv.map(([k, v]) => (
            <span key={k}>
              {k} <b>{v}</b>
            </span>
          ))}
        </div>
      )}
      {tools.length > 0 && (
        <div className="tool-chips" aria-label={t("mcp.tools", { tools: tools.join(", ") })}>
          {shown.map((name) => (
            <span key={name} className="tool-chip">
              {name}
            </span>
          ))}
          {tools.length > TOOL_CHIPS && (
            <button type="button" className="tool-chip more" onClick={() => setAllTools(!allTools)}>
              {allTools ? t("mcp.fewer") : t("mcp.more", { count: tools.length - TOOL_CHIPS })}
            </button>
          )}
        </div>
      )}
    </div>
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
