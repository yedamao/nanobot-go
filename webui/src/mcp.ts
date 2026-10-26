import type { MCPServer, MCPServerInput } from "./api.ts";
import { enT, type Translator } from "./i18n/translate.ts";

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
export function parsePairs(text: string, colon = false): Record<string, string> | string {
  const out: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line) continue;
    let i = line.indexOf("=");
    if (colon) {
      const c = line.indexOf(":");
      if (c >= 0 && (i < 0 || c < i)) i = c;
    }
    if (i <= 0) return 'line "' + line.split(/[=:]/)[0] + '…" needs ' + (colon ? "Name: value" : "KEY=VALUE");
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return out;
}

/** buildMCPInput turns the form into a PUT body, or an error message. */
export function buildMCPInput(form: MCPForm): { name: string; input: MCPServerInput } | string {
  const name = form.name.trim();
  if (!nameRE.test(name)) return "name must be 1-64 letters, digits, _ or -";
  const input: MCPServerInput = { enabled: form.enabled };
  if (form.kind === "stdio") {
    const command = form.command.trim();
    if (!command) return "command is required";
    input.command = command;
    const args = form.args.split("\n").map((a) => a.trim()).filter(Boolean);
    if (args.length) input.args = args;
    const env = parsePairs(form.env);
    if (typeof env === "string") return "env: " + env;
    if (Object.keys(env).length) input.env = env;
  } else {
    const url = form.url.trim();
    if (!/^https?:\/\/./.test(url)) return "url must start with http:// or https://";
    input.url = url;
    if (form.type) input.type = form.type;
    const headers = parsePairs(form.headers, true);
    if (typeof headers === "string") return "headers: " + headers;
    if (Object.keys(headers).length) input.headers = headers;
  }
  if (form.toolTimeout.trim()) {
    const n = Number(form.toolTimeout);
    if (!Number.isInteger(n) || n < 0) return "tool timeout must be a whole number of seconds";
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
