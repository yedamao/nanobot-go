import assert from "node:assert/strict";
import { test } from "node:test";
import { formatQuotedUserMessage, parseQuotedUserMessage, quoteSnippet } from "./quote.ts";

test("formatQuotedUserMessage blockquotes the context", () => {
  assert.equal(formatQuotedUserMessage(" why? ", "line one\r\n\r\nline two"), "> [!QUOTE]\n> line one\n>\n> line two\n\nwhy?");
  assert.equal(formatQuotedUserMessage("", "only quote"), "> [!QUOTE]\n> only quote");
  assert.equal(formatQuotedUserMessage("plain", null), "plain");
  assert.equal(formatQuotedUserMessage("plain", "   "), "plain");
});

test("formatQuotedUserMessage leaves commands unquoted", () => {
  assert.equal(formatQuotedUserMessage("/new", "context"), "/new");
});

test("parseQuotedUserMessage round-trips", () => {
  const msg = formatQuotedUserMessage("body\n\nmore", "a\n\nb");
  assert.deepEqual(parseQuotedUserMessage(msg), { quotedContext: "a\n\nb", content: "body\n\nmore" });
  assert.deepEqual(parseQuotedUserMessage("> [!QUOTE]\n> q"), { quotedContext: "q", content: "" });
});

test("parseQuotedUserMessage keeps other content as is", () => {
  for (const s of ["hello", "> [!QUOTE]", "> [!QUOTE]\nnot quoted\n\nbody", "> [!QUOTE]\n>\n\nbody", "> plain quote"]) {
    assert.deepEqual(parseQuotedUserMessage(s), { quotedContext: null, content: s });
  }
});

test("quoteSnippet flattens and shortens", () => {
  assert.equal(quoteSnippet("a\n  b"), "a b");
  assert.equal(quoteSnippet("你好世界", 2), "你好…");
});
