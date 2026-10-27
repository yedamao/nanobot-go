package telegram

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/bus"
)

// The cases follow nanobot's _markdown_to_telegram_html tests.
func TestMarkdownToTelegramHTML(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"h1", "# Title", "<b>Title</b>"},
		{"h2", "## Subtitle", "<b>Subtitle</b>"},
		{"h6", "###### Deep", "<b>Deep</b>"},
		{"header escaped", "# A < B & C > D", "<b>A &lt; B &amp; C &gt; D</b>"},
		{"bold italic strike", "**b** __u__ _i_ ~~s~~", "<b>b</b> <b>u</b> <i>i</i> <s>s</s>"},
		{"snake case stays", "some_var_name and _it_", "some_var_name and <i>it</i>"},
		{"escape", "a < b && c > d", "a &lt; b &amp;&amp; c &gt; d"},
		{"raw html escaped", "<b>not bold</b>", "&lt;b&gt;not bold&lt;/b&gt;"},
		{"inline code", "run `a < b` and `**x**`", "run <code>a &lt; b</code> and <code>**x**</code>"},
		{"blockquote", "> quoted\nnext", "quoted\nnext"},
		{"bullets", "- one\n* two", "• one\n• two"},
		{"numbered", "1.   Lots of space\n2.  Two spaces", "1. Lots of space\n2. Two spaces"},
		{"link", "see [docs](https://example.com/x) now", `see <a href="https://example.com/x">docs</a> now`},
		{"link url untouched", "[init](https://github.com/o/r/blob/main/pkg/__init__.py)",
			`<a href="https://github.com/o/r/blob/main/pkg/__init__.py">init</a>`},
		{"link url underscores", "see [css](https://example.com/_static_/a.css) ok",
			`see <a href="https://example.com/_static_/a.css">css</a> ok`},
		{"link url bold strike", "[x](https://example.com/a**b**c~~d~~)", `<a href="https://example.com/a**b**c~~d~~">x</a>`},
		{"link url quotes", `[q](https://example.com/?q="x"&y=1)`, `<a href="https://example.com/?q=&quot;x&quot;&amp;y=1">q</a>`},
		{"link text formatted", "[**bold** _it_](https://example.com/a_b_c)", `<a href="https://example.com/a_b_c"><b>bold</b> <i>it</i></a>`},
		{"fence", "```py\n# c\nx = __y__\n```", "<pre><code># c\nx = __y__\n</code></pre>"},
		{"fence language", "```c++\nint main() { return 0; }\n```", "<pre><code>int main() { return 0; }\n</code></pre>"},
		{"tilde fence", "Run:\n~~~python\n# setup\ndef __init__(self): pass\n~~~\nDone",
			"Run:\n<pre><code># setup\ndef __init__(self): pass\n</code></pre>\nDone"},
		{"fence escaped", "~~~\na < b && __raw__\n~~~", "<pre><code>a &lt; b &amp;&amp; __raw__\n</code></pre>"},
		{"longer fence nests", "Example:\n````markdown\n```python\nx = a * b\n```\n**raw**\n````\nAfter **bold**",
			"Example:\n<pre><code>```python\nx = a * b\n```\n**raw**\n</code></pre>\nAfter <b>bold</b>"},
		{"inline fence", "Use ```ls -la``` now", "Use <pre><code>ls -la</code></pre> now"},
		{"inline fence tag", "Use ```<tag>``` here", "Use <pre><code>&lt;tag&gt;</code></pre> here"},
		{"mixed", "# Overview\n\n- bullet one\n\n1. step one\n\n**bold text**",
			"<b>Overview</b>\n\n• bullet one\n\n1. step one\n\n<b>bold text</b>"},
		{"table", "| Name | Qty |\n|---|:-:|\n| **apple** | 3 |\n| kiwi | 12 |",
			"<pre><code>Name   Qty\n─────  ───\napple  3  \nkiwi   12 </code></pre>"},
		{"wide table", "| 名前 | n |\n|--|--|\n| a | 1 |", "<pre><code>名前  n\n────  ─\na     1</code></pre>"},
		{"not a table", "| just | pipes |", "| just | pipes |"},
	}
	for _, tt := range tests {
		if got := markdownToTelegramHTML(tt.in); got != tt.want {
			t.Errorf("%s: markdownToTelegramHTML(%q) =\n%q\nwant\n%q", tt.name, tt.in, got, tt.want)
		}
	}
}

func balanced(t *testing.T, chunks []string) {
	t.Helper()
	for _, c := range chunks {
		html := markdownToTelegramHTML(c)
		if strings.Count(html, "<pre><code>") != strings.Count(html, "</code></pre>") {
			t.Errorf("unbalanced code in %q", html)
		}
	}
}

// The cases follow nanobot's _split_telegram_markdown tests.
func TestSplitMarkdown(t *testing.T) {
	if got := splitMarkdown(" \n ", 10); got != nil {
		t.Errorf("whitespace = %q", got)
	}
	for _, prefix := range []string{"Intro paragraph.\n", "Intro paragraph. "} {
		chunks := splitMarkdown(prefix+"```python\nprint('a')\nprint('b')\n```\nDone", 35)
		if chunks[0] != prefix || !strings.HasPrefix(chunks[1], "```python\nprint('a')") {
			t.Errorf("cut not moved before the fence: %q", chunks)
		}
		balanced(t, chunks)
	}
	for _, fence := range []string{"```", "~~~", "````"} {
		chunks := splitMarkdown(fence+"python\n"+strings.Repeat("print('line one')\n", 6)+fence+"\nDone", 60)
		if len(chunks) < 2 || !strings.HasSuffix(chunks[0], "\n"+fence) || !strings.HasPrefix(chunks[1], fence+"python\n") {
			t.Errorf("%s: block not closed and reopened: %q", fence, chunks)
		}
		for _, c := range chunks {
			if utf16Len(c) > 60 {
				t.Errorf("%s: chunk of %d", fence, utf16Len(c))
			}
		}
		balanced(t, chunks)

		// A long body without newlines still advances.
		body := strings.Repeat("a", 4500)
		chunks = splitMarkdown(fence+"\n"+body+"\n"+fence, maxMarkdownLen)
		var got strings.Builder
		for _, c := range chunks {
			if utf16Len(c) > maxMarkdownLen || !strings.HasPrefix(c, fence+"\n") {
				t.Errorf("%s: chunk %q...", fence, c[:10])
			}
			part := strings.TrimPrefix(c, fence+"\n")
			part = strings.TrimSuffix(strings.TrimSuffix(part, fence), "\n")
			got.WriteString(part)
		}
		if len(chunks) < 2 || got.String() != body {
			t.Errorf("%s: body not kept across %d chunks", fence, len(chunks))
		}
		balanced(t, chunks)
	}

	nested := "````markdown\n```python\n" + strings.Repeat("# raw example\n", 40) + "```\n````"
	chunks := splitMarkdown(nested, 80)
	var html strings.Builder
	for _, c := range chunks {
		if utf16Len(c) > 80 || !strings.HasPrefix(c, "````markdown\n") || !strings.HasSuffix(c, "\n````") {
			t.Errorf("nested chunk %q", c)
		}
		html.WriteString(markdownToTelegramHTML(c))
	}
	if h := html.String(); strings.Count(h, "# raw example") != 40 || strings.Count(h, "```") != 2 || strings.Contains(h, "<b>") {
		t.Errorf("nested fence lost: %q", h)
	}

	// A limit as small as the fence and its closer hard-cuts.
	chunks = splitMarkdown("```\n"+strings.Repeat("a", 100), 8)
	for _, c := range chunks {
		if utf16Len(c) > 8 {
			t.Errorf("tiny chunk %q", c)
		}
	}
	if j := strings.NewReplacer("```", "", "\n", "").Replace(strings.Join(chunks, "")); j != strings.Repeat("a", 100) {
		t.Errorf("tiny limit lost text: %q", j)
	}

	// Limits are in UTF-16 units: an emoji counts two.
	chunks = splitMarkdown(strings.Repeat("😀", 10), 5)
	if !reflect.DeepEqual(chunks, []string{"😀😀", "😀😀", "😀😀", "😀😀", "😀😀"}) {
		t.Errorf("emoji chunks = %q", chunks)
	}
}

func TestSplitMarkdownHTMLFitsAfterExpansion(t *testing.T) {
	// Each & becomes &amp;: the raw 4000-unit chunks render far over the
	// limit and are split again.
	text := strings.Repeat("& ", 3000)
	chunks := splitMarkdownHTML(text, maxMessageLen)
	var raw []string
	for _, c := range chunks {
		if utf16Len(c.html) > maxMessageLen {
			t.Errorf("HTML chunk of %d units", utf16Len(c.html))
		}
		if c.html != markdownToTelegramHTML(c.raw) {
			t.Errorf("html is not the rendering of raw")
		}
		raw = append(raw, c.raw)
	}
	if strings.Join(raw, " ") != strings.TrimSpace(text) {
		t.Errorf("text lost across %d chunks", len(chunks))
	}
	// A long code block is closed and reopened, every chunk balanced.
	code := "```go\n" + strings.Repeat("if a < b && c > d {}\n", 400) + "```"
	chunks = splitMarkdownHTML(code, maxMessageLen)
	if len(chunks) < 2 {
		t.Fatalf("%d chunks", len(chunks))
	}
	for _, c := range chunks {
		if utf16Len(c.html) > maxMessageLen || !strings.HasPrefix(c.html, "<pre><code>") || !strings.HasSuffix(c.html, "</code></pre>") {
			t.Errorf("code chunk of %d units: %q...", utf16Len(c.html), c.html[:20])
		}
	}
}

const parseError = `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities: unclosed tag"}`

func TestSendHTMLFallsBackToPlainText(t *testing.T) {
	h := newHarness(t, nil)
	h.f.reply = func(method string, n int) (int, string, bool) {
		if method == "sendMessage" && n == 1 {
			return http.StatusBadRequest, parseError, true
		}
		return 0, "", false
	}
	h.start(t)
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "**hi** <there>"}); err != nil {
		t.Fatal(err)
	}
	sent := h.f.callsOf("sendMessage")
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sent))
	}
	if sent[0].Body["parse_mode"] != "HTML" || sent[0].Body["text"] != "<b>hi</b> &lt;there&gt;" {
		t.Errorf("first send = %v", sent[0].Body)
	}
	if _, ok := sent[1].Body["parse_mode"]; ok || sent[1].Body["text"] != "**hi** <there>" {
		t.Errorf("fallback = %v, want the raw text without parse_mode", sent[1].Body)
	}

	// Other rejections are not retried as plain text.
	h2 := newHarness(t, nil)
	h2.f.reply = func(method string, n int) (int, string, bool) {
		return http.StatusForbidden, `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`, method == "sendMessage"
	}
	h2.start(t)
	if err := h2.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "x"}); err == nil {
		t.Error("403 not returned")
	}
	if n := len(h2.f.callsOf("sendMessage")); n != 1 {
		t.Errorf("403: %d sends, want 1", n)
	}
}

func TestSendCaptionHTML(t *testing.T) {
	h := newHarness(t, nil)
	writeWS(t, h, "a.jpg", jpegFixture)
	writeWS(t, h, "b.png", pngFixture)
	h.f.reply = func(method string, n int) (int, string, bool) {
		// The second photo and the second album refuse their HTML.
		if (method == "sendPhoto" || method == "sendMediaGroup") && n == 2 {
			return http.StatusBadRequest, parseError, true
		}
		return 0, "", false
	}
	h.start(t)
	send := func(media ...string) {
		t.Helper()
		if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "**look** <here>", Media: media}); err != nil {
			t.Fatal(err)
		}
	}
	send("a.jpg")
	send("a.jpg")
	photos := h.f.callsOf("sendPhoto")
	if len(photos) != 3 || photos[0].Body["caption"] != "<b>look</b> &lt;here&gt;" || photos[0].Body["parse_mode"] != "HTML" {
		t.Fatalf("sendPhoto = %+v", photos)
	}
	if _, ok := photos[2].Body["parse_mode"]; ok || photos[2].Body["caption"] != "**look** <here>" {
		t.Errorf("caption fallback = %v", photos[2].Body)
	}

	send("a.jpg", "b.png")
	send("a.jpg", "b.png")
	albums := h.f.callsOf("sendMediaGroup")
	if len(albums) != 3 {
		t.Fatalf("%d albums, want 3", len(albums))
	}
	if it := albumItems(t, albums[0])[0]; it.Caption != "<b>look</b> &lt;here&gt;" || it.ParseMode != "HTML" {
		t.Errorf("album caption = %+v", it)
	}
	if it := albumItems(t, albums[2])[0]; it.Caption != "**look** <here>" || it.ParseMode != "" {
		t.Errorf("album caption fallback = %+v", it)
	}
	if n := len(h.f.callsOf("sendDocument")) + len(h.f.callsOf("sendMessage")); n != 0 {
		t.Errorf("%d documents or messages: a parse error is not a refused file", n)
	}

	// A caption whose HTML is over the limit follows as a message.
	long := strings.Repeat("&", 300) // 300 raw, 1500 as HTML
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: long, Media: []string{"a.jpg"}}); err != nil {
		t.Fatal(err)
	}
	if p := h.f.callsOf("sendPhoto")[3]; p.Body["caption"] != nil || p.Body["parse_mode"] != nil {
		t.Errorf("long caption on the photo: %v", p.Body)
	}
	if m := h.f.callsOf("sendMessage"); len(m) != 1 || m[0].Body["text"] != strings.Repeat("&amp;", 300) {
		t.Errorf("sendMessage = %+v", m)
	}
}
