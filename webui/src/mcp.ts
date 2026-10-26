import type { MCPServer, MCPServerInput, MCPTestResult } from "./api.ts";
import { enT, type Invalid, type Translator } from "./i18n/translate.ts";

/** The MCP page's add form. */
export interface MCPForm {
  name: string;
  kind: "stdio" | "http";
  command: string;
  /** One argument per line. */
  args: string;
  /** KEY=VALUE per line. */
  env: string;
  url: string;
  /** "" infers streamableHttp or sse from the url. */
  type: "" | "streamableHttp" | "sse";
  /** Name: value (or Name=value) per line. */
  headers: string;
  toolTimeout: string;
  /** Comma separated; empty exposes every tool. */
  enabledTools: string;
  enabled: boolean;
}

export const emptyMCPForm: MCPForm = {
  name: "",
  kind: "stdio",
  command: "",
  args: "",
  env: "",
  url: "",
  type: "",
  headers: "",
  toolTimeout: "",
  enabledTools: "",
  enabled: true,
};

const nameRE = /^[A-Za-z0-9_-]{1,64}$/;

/** parsePairs reads "KEY=VALUE" (or, with colon, "Name: value") lines. */
export function parsePairs(text: string, colon = false): { pairs: Record<string, string> } | { error: Invalid } {
  const pairs: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line) continue;
    let i = line.indexOf("=");
    if (colon) {
      const c = line.indexOf(":");
      if (c >= 0 && (i < 0 || c < i)) i = c;
    }
    if (i <= 0) {
      const vars = { line: line.split(/[=:]/)[0] };
      return { error: { key: colon ? "mcp.validation.headerLine" : "mcp.validation.envLine", vars } };
    }
    pairs[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return { pairs };
}

/** buildMCPInput turns the form into a PUT body, or why it cannot. */
export function buildMCPInput(form: MCPForm): { name: string; input: MCPServerInput } | { error: Invalid } {
  const name = form.name.trim();
  if (!nameRE.test(name)) return { error: { key: "mcp.validation.name" } };
  const input: MCPServerInput = { enabled: form.enabled };
  if (form.kind === "stdio") {
    const command = form.command.trim();
    if (!command) return { error: { key: "mcp.validation.commandRequired" } };
    input.command = command;
    const args = form.args.split("\n").map((a) => a.trim()).filter(Boolean);
    if (args.length) input.args = args;
    const env = parsePairs(form.env);
    if ("error" in env) return env;
    if (Object.keys(env.pairs).length) input.env = env.pairs;
  } else {
    const url = form.url.trim();
    if (!/^https?:\/\/./.test(url)) return { error: { key: "mcp.validation.url" } };
    input.url = url;
    if (form.type) input.type = form.type;
    const headers = parsePairs(form.headers, true);
    if ("error" in headers) return headers;
    if (Object.keys(headers.pairs).length) input.headers = headers.pairs;
  }
  if (form.toolTimeout.trim()) {
    const n = Number(form.toolTimeout);
    if (!Number.isInteger(n) || n < 0) return { error: { key: "mcp.validation.toolTimeout" } };
    if (n > 0) input.toolTimeout = n;
  }
  const tools = form.enabledTools.split(",").map((t) => t.trim()).filter(Boolean);
  if (tools.length) input.enabledTools = tools;
  return { name, input };
}

/** mcpTarget is the one-line description of what a server runs or calls. */
export function mcpTarget(s: MCPServer): string {
  if (s.command) return [s.command, ...(s.args ?? [])].join(" ");
  return s.url ?? "";
}

/** mcpBadges are the badges shown next to a server's name. */
export function mcpBadges(s: MCPServer, t: Translator = enT): string[] {
  const out = [s.transport];
  if (s.removed) out.push(t("mcp.badges.removed"));
  else if (!s.enabled) out.push(t("common.disabled"));
  if (s.restart_required) out.push(t("common.restartRequired"));
  return out;
}

/** canTestMCP reports whether a server can be tested: saved and enabled
 * (the gateway does not dial disabled servers, nor does the test). */
export function canTestMCP(s: MCPServer): boolean {
  return !s.removed && s.enabled;
}

/** mcpTestNote is a card's note for a test's result. */
export function mcpTestNote(r: MCPTestResult, t: Translator = enT): { kind: "ok" | "error"; text: string } {
  const message = r.message || t(r.ok ? "mcp.testConnected" : "common.testFailedDefault");
  return { kind: r.ok ? "ok" : "error", text: t(r.ok ? "common.testPassed" : "common.testFailed", { message }) };
}
