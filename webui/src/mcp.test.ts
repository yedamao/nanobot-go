import assert from "node:assert/strict";
import { test } from "node:test";
import type { MCPServer } from "./api.ts";
import { buildMCPInput, emptyMCPForm, mcpBadges, mcpTarget, parsePairs } from "./mcp.ts";

test("parsePairs reads env and header lines", () => {
  assert.deepEqual(parsePairs("A=1\n\n B = x=y \n"), { A: "1", B: "x=y" });
  assert.deepEqual(parsePairs("Authorization: Bearer abc\nX-Key=v", true), { Authorization: "Bearer abc", "X-Key": "v" });
  assert.equal(typeof parsePairs("novalue"), "string");
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
  assert.equal(buildMCPInput({ ...emptyMCPForm, name: "bad name", command: "x" }), "name must be 1-64 letters, digits, _ or -");
  assert.equal(buildMCPInput({ ...emptyMCPForm, name: "x" }), "command is required");
  assert.equal(buildMCPInput({ ...emptyMCPForm, name: "x", kind: "http", url: "ftp://a" }), "url must start with http:// or https://");
  assert.equal(buildMCPInput({ ...emptyMCPForm, name: "x", command: "c", toolTimeout: "1.5" }), "tool timeout must be a whole number of seconds");
});

test("target and badges describe a server", () => {
  const s: MCPServer = { name: "fs", enabled: false, transport: "stdio", command: "npx", args: ["-y", "srv"], enabled_tools: null, state: "disabled", restart_required: true };
  assert.equal(mcpTarget(s), "npx -y srv");
  assert.deepEqual(mcpBadges(s), ["stdio", "disabled", "restart required"]);
  assert.equal(mcpTarget({ ...s, command: undefined, url: "https://h.test" }), "https://h.test");
  assert.deepEqual(mcpBadges({ ...s, removed: true, restart_required: true }), ["stdio", "removed", "restart required"]);
});
