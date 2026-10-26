import assert from "node:assert/strict";
import { test } from "node:test";
import type { MCPServer } from "./api.ts";
import { buildMCPInput, canTestMCP, emptyMCPForm, mcpBadges, mcpTarget, mcpTestNote, parsePairs } from "./mcp.ts";

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
