// Run with `npm test` (Node's built-in runner; Node strips the types).
import assert from "node:assert/strict";
import { test } from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ReactMarkdown from "react-markdown";
import { markdownOptions, safeUrl } from "./markdown-options.ts";

const render = (src: string) => renderToStaticMarkup(createElement(ReactMarkdown, markdownOptions, src));

test("raw HTML is not rendered", () => {
  for (const src of [
    "<script>alert(1)</script>",
    "hi <img src=x onerror=alert(1)> there",
    "<iframe src=//evil></iframe>\n\ntext",
    "<a href=\"javascript:alert(1)\">x</a>",
  ]) {
    const html = render(src);
    assert.doesNotMatch(html, /<(script|img|iframe)|onerror|javascript:/i, `${src} → ${html}`);
  }
});

test("unsafe link schemes are dropped", () => {
  for (const src of [
    "[x](javascript:alert(1))",
    "[x](JaVaScRiPt:alert(1))",
    "[x](&#106;avascript:alert(1))",
    "[x](data:text/html,<script>alert(1)</script>)",
    "[x](vbscript:msgbox)",
    "<javascript:alert(1)>",
    "![x](javascript:alert(1))",
  ]) {
    const html = render(src);
    assert.doesNotMatch(html, /href=|<img/i, `${src} → ${html}`);
  }
});

test("safe links open in a new tab; images become links", () => {
  assert.equal(
    render("[a](https://example.com/x?y=1)"),
    '<p><a href="https://example.com/x?y=1" target="_blank" rel="noopener noreferrer">a</a></p>',
  );
  assert.match(render("[m](mailto:a@b.c)"), /href="mailto:a@b.c"/);
  assert.equal(render("[s](#sec)"), '<p><a href="#sec">s</a></p>');
  assert.equal(
    render("![cat](https://example.com/c.png)"),
    '<p><a href="https://example.com/c.png" target="_blank" rel="noopener noreferrer">cat</a></p>',
  );
});

test("safeUrl", () => {
  assert.equal(safeUrl("https://a.b"), "https://a.b");
  assert.equal(safeUrl("./x:y"), "./x:y");
  assert.equal(safeUrl("/p?q=a:b"), "/p?q=a:b");
  assert.equal(safeUrl(" javascript:x"), "");
  assert.equal(safeUrl("java\tscript:x"), "");
  assert.equal(safeUrl("ftp://x"), "");
});

test("GFM and highlighted fences", () => {
  const html = render("| a | b |\n|---|---|\n| 1 | ~~2~~ |\n\n- [x] done\n\n```js\nconst x = 1;\n```\n\n```nolang\nplain\n```");
  assert.match(html, /<table>/);
  assert.match(html, /<del>2<\/del>/);
  assert.match(html, /type="checkbox"/);
  assert.match(html, /<code class="hljs language-js"><span class="hljs-keyword">const<\/span>/);
  assert.match(html, /<code class="(hljs )?language-nolang">plain\n<\/code>/);
});

test("single newlines break lines; unclosed fences are code", () => {
  assert.equal(render("a\nb"), "<p>a<br/>\nb</p>");
  assert.match(render("text\n\n```py\nprint(1"), /<pre><code class="hljs language-py">/);
});
