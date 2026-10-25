import { useCallback, useEffect, useState, type FormEvent } from "react";
import { deleteMCPServer, listMCP, putMCPServer, setMCPServerEnabled, type MCPList, type MCPServer } from "./api";
import { buildMCPInput, emptyMCPForm, mcpBadges, mcpTarget, type MCPForm } from "./mcp";
import { RestartGateway } from "./RestartGateway";

export function MCPPage() {
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
      setNote("Failed: " + (err as Error).message);
      return false;
    } finally {
      setBusy(null);
    }
  };

  const toggle = (s: MCPServer) =>
    act(s.name, (s.enabled ? "Disabled " : "Enabled ") + s.name, () => setMCPServerEnabled(s.name, !s.enabled));
  const remove = (s: MCPServer) => {
    if (!confirm('Remove MCP server "' + s.name + '" from the config?')) return;
    void act(s.name, "Removed " + s.name, () => deleteMCPServer(s.name));
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const built = buildMCPInput(form);
    if (typeof built === "string") {
      setNote("Cannot add: " + built);
      return;
    }
    if (list?.servers.some((s) => s.name === built.name && !s.removed)) {
      if (!confirm('Replace the MCP server "' + built.name + '"?')) return;
    }
    if (await act("+add", "Saved " + built.name, () => putMCPServer(built.name, built.input))) {
      setForm(emptyMCPForm);
    }
  };

  const set = (patch: Partial<MCPForm>) => setForm({ ...form, ...patch });

  return (
    <section className="page" aria-labelledby="mcp-heading">
      <header className="page-header">
        <h2 id="mcp-heading">MCP servers</h2>
        <button type="button" onClick={() => void load()} disabled={busy !== null}>
          Refresh
        </button>
      </header>
      {error && <p className="page-error">Cannot load MCP servers ({error}).</p>}
      {note && <p className="page-note">{note}</p>}
      {list && list.restart_required.length > 0 && (
        <p className="page-note">
          Restart the gateway to apply changes to: {list.restart_required.join(", ")}. MCP servers connect when the
          gateway starts.
        </p>
      )}
      {list && list.restart_required.length > 0 && <RestartGateway />}
      {!error && list === null && <p className="page-empty">Loading…</p>}
      {list &&
        (list.servers.length === 0 ? (
          <p className="page-empty">
            No MCP servers configured. Add one below; it is saved to <code>tools.mcpServers</code>.
          </p>
        ) : (
          <ul className="page-list skills-list">
            {list.servers.map((s) => (
              <li key={s.name} data-disabled={s.enabled ? undefined : ""}>
                <div className="skill-head">
                  <strong>{s.name}</strong>
                  {mcpBadges(s).map((b) => (
                    <span key={b} className="badge">
                      {b}
                    </span>
                  ))}
                  {list.live && (
                    <span className="badge" data-state={s.state}>
                      {s.state.replace("_", " ")}
                    </span>
                  )}
                  {!s.removed && (
                    <span className="page-actions">
                      <button type="button" disabled={busy !== null} onClick={() => void toggle(s)}>
                        {s.enabled ? "Disable" : "Enable"}
                      </button>
                      <button type="button" disabled={busy !== null} onClick={() => remove(s)}>
                        Remove
                      </button>
                    </span>
                  )}
                </div>
                <code className="skill-path">{mcpTarget(s)}</code>
                {!!(s.env_keys?.length || s.header_keys?.length || s.enabled_tools || s.tool_timeout) && (
                  <p>
                    {s.env_keys?.length ? "env: " + s.env_keys.join(", ") + " (values hidden). " : ""}
                    {s.header_keys?.length ? "headers: " + s.header_keys.join(", ") + " (values hidden). " : ""}
                    {s.enabled_tools ? "tools allowed: " + (s.enabled_tools.join(", ") || "none") + ". " : ""}
                    {s.tool_timeout ? "timeout " + s.tool_timeout + "s." : ""}
                  </p>
                )}
                {s.error && <p className="page-error">{s.error}</p>}
                {s.tools && s.tools.length > 0 && <p>Tools: {s.tools.join(", ")}</p>}
              </li>
            ))}
          </ul>
        ))}

      <form className="cron-form" onSubmit={(e) => void submit(e)} autoComplete="off">
        <h3>Add MCP server</h3>
        <fieldset className="cron-fields">
          <label htmlFor="mcp-name">
            Name
            <input id="mcp-name" type="text" required value={form.name} placeholder="e.g. filesystem" onChange={(e) => set({ name: e.target.value })} />
          </label>
          <label htmlFor="mcp-kind">
            Transport
            <select id="mcp-kind" value={form.kind} onChange={(e) => set({ kind: e.target.value as MCPForm["kind"] })}>
              <option value="stdio">stdio (command)</option>
              <option value="http">HTTP (url)</option>
            </select>
          </label>
          {form.kind === "stdio" ? (
            <>
              <label htmlFor="mcp-command">
                Command
                <input id="mcp-command" type="text" value={form.command} placeholder="npx" onChange={(e) => set({ command: e.target.value })} />
              </label>
              <label htmlFor="mcp-args">
                Args
                <textarea id="mcp-args" rows={3} value={form.args} placeholder="one per line" onChange={(e) => set({ args: e.target.value })} />
              </label>
              <label htmlFor="mcp-env">
                Env
                <textarea id="mcp-env" rows={2} value={form.env} placeholder="KEY=value or KEY=${VAR}, one per line" onChange={(e) => set({ env: e.target.value })} />
              </label>
            </>
          ) : (
            <>
              <label htmlFor="mcp-url">
                URL
                <input id="mcp-url" type="text" value={form.url} placeholder="https://host/mcp" onChange={(e) => set({ url: e.target.value })} />
              </label>
              <label htmlFor="mcp-type">
                Type
                <select id="mcp-type" value={form.type} onChange={(e) => set({ type: e.target.value as MCPForm["type"] })}>
                  <option value="">auto (…/sse means sse)</option>
                  <option value="streamableHttp">streamableHttp</option>
                  <option value="sse">sse</option>
                </select>
              </label>
              <label htmlFor="mcp-headers">
                Headers
                <textarea id="mcp-headers" rows={2} value={form.headers} placeholder="Authorization: Bearer ${TOKEN}" onChange={(e) => set({ headers: e.target.value })} />
              </label>
            </>
          )}
          <label htmlFor="mcp-tools">
            Tools
            <input id="mcp-tools" type="text" value={form.enabledTools} placeholder="all (or comma-separated names)" onChange={(e) => set({ enabledTools: e.target.value })} />
          </label>
          <label htmlFor="mcp-timeout">
            Timeout (s)
            <input id="mcp-timeout" type="number" min={0} step={1} value={form.toolTimeout} placeholder="30" onChange={(e) => set({ toolTimeout: e.target.value })} />
          </label>
          <label className="check">
            <input type="checkbox" checked={form.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
            Enabled
          </label>
        </fieldset>
        <div className="cron-form-actions">
          <button type="submit" disabled={busy !== null}>
            Save
          </button>
        </div>
      </form>
      <p className="page-empty">
        Env and header values are saved to the config file and never shown here; prefer <code>${"{VAR}"}</code>{" "}
        references so secrets stay in the environment. Changes apply when the gateway restarts.
      </p>
    </section>
  );
}
