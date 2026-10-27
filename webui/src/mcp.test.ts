import assert from "node:assert/strict";
import { test } from "node:test";
import type { MCPServer } from "./api.ts";
import { buildMCPInput, canAuthorizeMCP, canTestMCP, emptyMCPForm, mcpBadges, mcpOAuthNote, mcpTarget, mcpTestNote, oauthFlowActive, parsePairs } from "./mcp.ts";

test("parsePairs reads env and header lines", () => {
  assert.deepEqual(parsePairs("A=1\n\n B = x=y \n"), { pairs: { A: "1", B: "x=y" } });
  assert.deepEqual(parsePairs("Authorization: Bearer abc\nX-Key=v", true), { pairs: { Authorization: "Bearer abc", "X-Key": "v" } });
  assert.deepEqual(parsePairs("novalue"), { error: { key: "mcp.validation.envLine", vars: { line: "novalue" } } });
  assert.deepEqual(parsePairs("Bad", true), { error: { key: "mcp.validation.headerLine", vars: { line: "Bad" } } });
});

test("buildMCPInput builds stdio and http bodies", () => {
  assert.deepEqual(
    buildMCPInput({ ...emptyMCPForm, name: "fs", command: " npx ", args: "-y\n\n@scope/server\n", env: "TOKEN=${GH}", enabledTools: "read, write" }),
    { name: "fs", input: { enabled: true, command: "npx", args: ["-y", "@scope/server"], env: { TOKEN: "${GH}" }, enabledTools: ["read", "write"] } },
  );
  assert.deepEqual(
    buildMCPInput({ ...emptyMCPForm, name: "web", kind: "http", url: "https://h.test/sse", type: "sse", toolTimeout: "10", enabled: false, command: "ignored" }),
    { name: "web", input: { enabled: false, url: "https://h.test/sse", type: "sse", toolTimeout: 10 } },
  );
  assert.deepEqual(buildMCPInput({ ...emptyMCPForm, name: "bad name", command: "x" }), { error: { key: "mcp.validation.name" } });
  assert.deepEqual(buildMCPInput({ ...emptyMCPForm, name: "x" }), { error: { key: "mcp.validation.commandRequired" } });
  assert.deepEqual(buildMCPInput({ ...emptyMCPForm, name: "x", kind: "http", url: "ftp://a" }), { error: { key: "mcp.validation.url" } });
  assert.deepEqual(buildMCPInput({ ...emptyMCPForm, name: "x", command: "c", toolTimeout: "1.5" }), { error: { key: "mcp.validation.toolTimeout" } });
  assert.deepEqual(buildMCPInput({ ...emptyMCPForm, name: "x", command: "c", env: "NOPE" }), { error: { key: "mcp.validation.envLine", vars: { line: "NOPE" } } });
});

test("target and badges describe a server", () => {
  const s: MCPServer = { name: "fs", enabled: false, transport: "stdio", command: "npx", args: ["-y", "srv"], enabled_tools: null, state: "disabled", restart_required: true };
  assert.equal(mcpTarget(s), "npx -y srv");
  assert.deepEqual(mcpBadges(s), ["stdio", "disabled", "restart required"]);
  assert.equal(mcpTarget({ ...s, command: undefined, url: "https://h.test" }), "https://h.test");
  assert.deepEqual(mcpBadges({ ...s, removed: true, restart_required: true }), ["stdio", "removed", "restart required"]);
});

test("canTestMCP tests only saved, enabled servers", () => {
  const s: MCPServer = { name: "fs", enabled: true, transport: "stdio", enabled_tools: null, state: "unknown", restart_required: false };
  assert.equal(canTestMCP(s), true);
  assert.equal(canTestMCP({ ...s, enabled: false }), false);
  assert.equal(canTestMCP({ ...s, removed: true }), false);
});

test("mcpTestNote", () => {
  assert.deepEqual(mcpTestNote({ ok: true, status: "ok", message: "fs connected with 2 tools.", tools: ["a", "b"] }), {
    kind: "ok",
    text: "Test passed: fs connected with 2 tools.",
  });
  assert.deepEqual(mcpTestNote({ ok: true, status: "ok", message: "" }), { kind: "ok", text: "Test passed: The server connected." });
  assert.deepEqual(mcpTestNote({ ok: false, status: "rejected", message: "web refused access (HTTP 401/403): check its headers." }), {
    kind: "error",
    text: "Test failed: web refused access (HTTP 401/403): check its headers.",
  });
  assert.deepEqual(mcpTestNote({ ok: false, status: "error", message: "" }), { kind: "error", text: "Test failed: The test failed." });
});

test("buildMCPInput sends auth oauth instead of headers", () => {
  const form = { ...emptyMCPForm, name: "linear", kind: "http" as const, url: "https://mcp.linear.app/mcp", auth: "oauth" as const, headers: "Authorization: Bearer x" };
  assert.deepEqual(buildMCPInput(form), { name: "linear", input: { enabled: true, url: "https://mcp.linear.app/mcp", auth: "oauth" } });
  const headers = buildMCPInput({ ...form, auth: "headers" });
  assert.ok("input" in headers && headers.input.auth === undefined && headers.input.headers?.Authorization === "Bearer x");
});

test("OAuth servers get a badge and can be signed in", () => {
  const s = { name: "linear", enabled: true, transport: "streamableHttp", enabled_tools: null, state: "authorization_required", restart_required: false, auth: "oauth", oauth: "authorization_required" as const };
  assert.deepEqual(mcpBadges(s), ["streamableHttp", "OAuth"]);
  assert.equal(canAuthorizeMCP(s), true);
  assert.equal(canAuthorizeMCP({ ...s, removed: true }), false);
  assert.equal(canAuthorizeMCP({ ...s, auth: undefined }), false);
});

test("mcpOAuthNote follows the flow", () => {
  const f = { flow_id: "f", name: "linear", status: "authorization_required" as const, expires_in: 300 };
  assert.equal(oauthFlowActive(f), true);
  assert.equal(mcpOAuthNote(f).kind, "");
  assert.match(mcpOAuthNote({ ...f, completion_input: "callback_url" }).text, /paste the callback URL/);
  assert.deepEqual(mcpOAuthNote({ ...f, status: "connected" }), { kind: "ok", text: "linear is signed in and connected." });
  const authorized = { ...f, status: "authorized" as const, hot_reload: { ok: false, message: "Signed in. Restart the gateway to connect linear.", requires_restart: true } };
  assert.equal(oauthFlowActive(authorized), false);
  assert.equal(mcpOAuthNote(authorized).text, "Signed in. Restart the gateway to connect linear.");
  assert.deepEqual(mcpOAuthNote({ ...f, status: "failed", error: "Authorization was not completed (access_denied)." }), {
    kind: "error",
    text: "Sign-in failed: Authorization was not completed (access_denied).",
  });
});
