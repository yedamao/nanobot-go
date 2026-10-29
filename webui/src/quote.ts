// Reply quotes travel inside a user message's content, as in nanobot's
// webui (user-message-quote.ts): a "> [!QUOTE]" line, the quoted text as
// a blockquote, a blank line, then the message.

export interface ParsedQuote {
  quotedContext: string | null;
  content: string;
}

const QUOTED_CONTEXT_MARKER = "> [!QUOTE]";

function normalizeNewlines(value: string): string {
  return value.replace(/\r\n?/g, "\n");
}

// formatQuotedUserMessage puts quotedContext ahead of content. Commands
// ("/new") go unquoted.
export function formatQuotedUserMessage(content: string, quotedContext?: string | null): string {
  const body = content.trim();
  const quote = normalizeNewlines(quotedContext ?? "").trim();
  if (!quote || body.startsWith("/")) return body;
  const blockquote = quote
    .split("\n")
    .map((line) => (line ? `> ${line}` : ">"))
    .join("\n");
  const quoted = `${QUOTED_CONTEXT_MARKER}\n${blockquote}`;
  return body ? `${quoted}\n\n${body}` : quoted;
}

// parseQuotedUserMessage splits a formatted message back into its quote
// and body; other content comes back as is.
export function parseQuotedUserMessage(content: string): ParsedQuote {
  const normalized = normalizeNewlines(content);
  if (!normalized.startsWith(`${QUOTED_CONTEXT_MARKER}\n`)) return { quotedContext: null, content };
  const start = QUOTED_CONTEXT_MARKER.length + 1;
  const sep = normalized.indexOf("\n\n", start);
  const lines = (sep === -1 ? normalized.slice(start) : normalized.slice(start, sep)).split("\n");
  if (lines.some((line) => line !== ">" && !line.startsWith("> "))) return { quotedContext: null, content };
  const quotedContext = lines
    .map((line) => (line === ">" ? "" : line.slice(2)))
    .join("\n")
    .trim();
  if (!quotedContext) return { quotedContext: null, content };
  return { quotedContext, content: sep === -1 ? "" : normalized.slice(sep + 2) };
}

// quoteSnippet shortens a quote for the composer's preview.
export function quoteSnippet(text: string, max = 140): string {
  const flat = text.replace(/\s+/g, " ").trim();
  const chars = Array.from(flat);
  return chars.length > max ? chars.slice(0, max).join("") + "…" : flat;
}
