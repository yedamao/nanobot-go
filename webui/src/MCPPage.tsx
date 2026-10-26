import { useCallback, useEffect, useState, type FormEvent } from "react";
import { deleteMCPServer, listMCP, putMCPServer, setMCPServerEnabled, type MCPList, type MCPServer } from "./api";
import { buildMCPInput, emptyMCPForm, mcpBadges, mcpTarget, type MCPForm } from "./mcp";
import { RestartGateway } from "./RestartGateway";
import { Fill, useT, type MessageKey } from "./i18n";

export function MCPPage() {
  const t = useT();
  const [list, setList] = useState<MCPList | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState<string | null>(null);
  const [note, setNote] = useState("");
  const [form, setForm] = useState<MCPForm>(emptyMCPForm);

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
                      <button type="button" disabled={busy !== null} onClick={() => void toggle(s)}>
                        {t(s.enabled ? "common.disable" : "common.enable")}
                      </button>
                      <button type="button" disabled={busy !== null} onClick={() => remove(s)}>
                        {t("common.remove")}
                      </button>
                    </span>
                  )}
                </div>
                <code className="skill-path">{mcpTarget(s)}</code>
                {!!(s.env_keys?.length || s.header_keys?.length || s.enabled_tools || s.tool_timeout) && (
                  <p>
                    {s.env_keys?.length ? t("mcp.env", { keys: s.env_keys.join(", ") }) : ""}
                    {s.header_keys?.length ? t("mcp.headers", { keys: s.header_keys.join(", ") }) : ""}
                    {s.enabled_tools ? t("mcp.toolsAllowed", { tools: s.enabled_tools.join(", ") || t("common.none") }) : ""}
                    {s.tool_timeout ? t("mcp.timeout", { seconds: s.tool_timeout }) : ""}
                  </p>
                )}
                {s.error && <p className="page-error">{s.error}</p>}
                {s.tools && s.tools.length > 0 && <p>{t("mcp.tools", { tools: s.tools.join(", ") })}</p>}
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
              <label htmlFor="mcp-headers">
                {t("mcp.fields.headers")}
                <textarea id="mcp-headers" rows={2} value={form.headers} placeholder="Authorization: Bearer ${TOKEN}" onChange={(e) => set({ headers: e.target.value })} />
              </label>
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
