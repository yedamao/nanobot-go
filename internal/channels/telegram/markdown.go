package telegram

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Outbound text is Markdown rendered to Telegram HTML (parse_mode=HTML),
// ported from nanobot's _markdown_to_telegram_html and
// _split_telegram_markdown. Lengths are counted in UTF-16 code units, as
// Telegram counts them, where nanobot counts code points.

// maxMarkdownLen is the budget raw Markdown is first split to, below
// maxMessageLen (nanobot's TELEGRAM_MAX_MESSAGE_LEN) as a margin; a chunk
// whose HTML still exceeds maxMessageLen is split again, smaller.
const maxMarkdownLen = 4000

var (
	fenceOpenRE   = regexp.MustCompile("^[ \t]*(`{3,}|~{3,})(.*)$")
	inlineFenceRE = regexp.MustCompile("```(?:[^\n]*\n)?([\\s\\S]*?)```")
	tableLineRE   = regexp.MustCompile(`^\s*\|.+\|`)
	tableSepRE    = regexp.MustCompile(`^:?-+:?$`)
	inlineCodeRE  = regexp.MustCompile("`([^`]+)`")
	headerRE      = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	quoteRE       = regexp.MustCompile(`(?m)^>\s*(.*)$`)
	linkRE        = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	boldStarRE    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	boldUnderRE   = regexp.MustCompile(`__(.+?)__`)
	strikeRE      = regexp.MustCompile(`~~(.+?)~~`)
	bulletRE      = regexp.MustCompile(`(?m)^[-*]\s+`)
	numberedRE    = regexp.MustCompile(`(?m)^(\d+)\.\s+`)
)

// openingCodeFence returns the fence (``` or ~~~, three or more) a line
// opens, or "" when it opens none. A backtick fence whose info string
// holds a backtick is inline code, not a fence.
func openingCodeFence(line string) string {
	m := fenceOpenRE.FindStringSubmatch(line)
	if m == nil || m[1][0] == '`' && strings.Contains(m[2], "`") {
		return ""
	}
	return m[1]
}

// closesCodeFence reports whether line closes fence: only a run of the
// same character, at least as long.
func closesCodeFence(line, fence string) bool {
	closing := strings.TrimSpace(line)
	return len(closing) >= len(fence) && closing == strings.Repeat(fence[:1], len(closing))
}

// escapeHTML escapes text for Telegram's HTML parse mode.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

// stripMD removes inline formatting from a table cell.
func stripMD(s string) string {
	s = boldStarRE.ReplaceAllString(s, "$1")
	s = boldUnderRE.ReplaceAllString(s, "$1")
	s = strikeRE.ReplaceAllString(s, "$1")
	s = inlineCodeRE.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

// replaceFencedCode replaces each fenced code block with replace(code).
// A block opens on a ``` or ~~~ line and closes on a line of only the
// same character, at least as long, so a ```` fence can show a ```
// example. One-line ```code``` (and what an unclosed fence leaves) is
// matched by inlineFenceRE.
func replaceFencedCode(text string, replace func(code string) string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		fence := openingCodeFence(lines[i])
		closed := false
		if fence != "" {
			for j := i + 1; j < len(lines); j++ {
				if closesCodeFence(lines[j], fence) {
					var code strings.Builder
					for _, l := range lines[i+1 : j] {
						code.WriteString(l + "\n")
					}
					out = append(out, replace(code.String()))
					i, closed = j+1, true
					break
				}
			}
		}
		if !closed {
			out = append(out, lines[i])
			i++
		}
	}
	text = strings.Join(out, "\n")
	return inlineFenceRE.ReplaceAllStringFunc(text, func(m string) string {
		return replace(inlineFenceRE.FindStringSubmatch(m)[1])
	})
}

// displayWidth is the width of s in a monospace font, wide and
// fullwidth characters counting two.
func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		if wideRune(r) {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// wideRune approximates Unicode East Asian Width W or F (nanobot's
// unicodedata.east_asian_width) with the main blocks: Hangul Jamo, CJK,
// Hangul syllables, compatibility ideographs, fullwidth forms and the
// emoji planes.
func wideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F,
		r >= 0x2E80 && r <= 0x303E,
		r >= 0x3041 && r <= 0x33FF,
		r >= 0x3400 && r <= 0x4DBF,
		r >= 0x4E00 && r <= 0x9FFF,
		r >= 0xA000 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE4F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F,
		r >= 0x1F680 && r <= 0x1F6FF,
		r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x3FFFD:
		return true
	}
	return false
}

// renderTableBox renders a Markdown pipe table as aligned text for a
// <pre> block: the header, a rule of ─, then the rows. Lines that are
// not a table (no separator row) come back unchanged.
func renderTableBox(lines []string) string {
	var rows [][]string
	hasSep := false
	for _, line := range lines {
		parts := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		cells := make([]string, len(parts))
		sep := true
		for i, p := range parts {
			cells[i] = stripMD(p)
			if cells[i] != "" && !tableSepRE.MatchString(cells[i]) {
				sep = false
			}
		}
		if sep {
			hasSep = true
			continue
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 || !hasSep {
		return strings.Join(lines, "\n")
	}
	ncols := 0
	for _, r := range rows {
		ncols = max(ncols, len(r))
	}
	widths := make([]int, ncols)
	for i, r := range rows {
		for len(r) < ncols {
			r = append(r, "")
		}
		rows[i] = r
		for c, cell := range r {
			widths[c] = max(widths[c], displayWidth(cell))
		}
	}
	row := func(cells []string) string {
		out := make([]string, len(cells))
		for c, cell := range cells {
			out[c] = cell + strings.Repeat(" ", widths[c]-displayWidth(cell))
		}
		return strings.Join(out, "  ")
	}
	out := []string{row(rows[0])}
	rule := make([]string, ncols)
	for c, w := range widths {
		rule[c] = strings.Repeat("─", w)
	}
	out = append(out, strings.Join(rule, "  "))
	for _, r := range rows[1:] {
		out = append(out, row(r))
	}
	return strings.Join(out, "\n")
}

// isASCIIAlnum is Python's [a-zA-Z0-9].
func isASCIIAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// italicize turns _text_ into <i>text</i> where neither underscore
// touches a letter or digit outside it, so some_var_name stays as it is
// (nanobot's (?<![a-zA-Z0-9])_([^_]+)_(?![a-zA-Z0-9]), which RE2
// cannot express).
func italicize(s string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '_' || i > 0 && isASCIIAlnum(s[i-1]) {
			continue
		}
		j := strings.IndexByte(s[i+1:], '_')
		if j <= 0 { // no closing underscore, or nothing between
			continue
		}
		end := i + 1 + j
		if end+1 < len(s) && isASCIIAlnum(s[end+1]) {
			continue
		}
		b.WriteString(s[last:i])
		b.WriteString("<i>" + s[i+1:end] + "</i>")
		last = end + 1
		i = end
	}
	b.WriteString(s[last:])
	return b.String()
}

// markdownToTelegramHTML renders Markdown as Telegram HTML, like
// nanobot's _markdown_to_telegram_html: fenced code and tables become
// <pre><code>, inline code <code>, headers and **bold**/__bold__ <b>,
// _italic_ <i>, ~~strike~~ <s>, links <a href>, list bullets •, and
// blockquote markers are dropped. Everything else is HTML-escaped.
func markdownToTelegramHTML(text string) string {
	if text == "" {
		return ""
	}
	// 1. Park fenced code, untouched by the passes below.
	var codeBlocks []string
	saveBlock := func(code string) string {
		codeBlocks = append(codeBlocks, code)
		return "\x00CB" + strconv.Itoa(len(codeBlocks)-1) + "\x00"
	}
	text = replaceFencedCode(text, saveBlock)

	// 1.5. Tables become aligned text, parked as code blocks.
	lines := strings.Split(text, "\n")
	rebuilt := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		if !tableLineRE.MatchString(lines[i]) {
			rebuilt = append(rebuilt, lines[i])
			i++
			continue
		}
		var tbl []string
		for i < len(lines) && tableLineRE.MatchString(lines[i]) {
			tbl = append(tbl, lines[i])
			i++
		}
		if box := renderTableBox(tbl); box != strings.Join(tbl, "\n") {
			rebuilt = append(rebuilt, saveBlock(box))
		} else {
			rebuilt = append(rebuilt, tbl...)
		}
	}
	text = strings.Join(rebuilt, "\n")

	// 2. Park inline code.
	var inlineCodes []string
	text = inlineCodeRE.ReplaceAllStringFunc(text, func(m string) string {
		inlineCodes = append(inlineCodes, m[1:len(m)-1])
		return "\x00IC" + strconv.Itoa(len(inlineCodes)-1) + "\x00"
	})

	// 3. Headers become bold, through markers that survive escaping.
	text = headerRE.ReplaceAllString(text, "⟪B⟫${1}⟪/B⟫")
	// 4. Blockquote markers go; their text stays.
	text = quoteRE.ReplaceAllString(text, "${1}")
	// 5. Escape.
	text = escapeHTML(text)

	// 6. Links, before bold and italic; the URL is parked so those passes
	// leave it alone (pkg/__init__.py must not gain a <b>).
	var linkURLs []string
	text = linkRE.ReplaceAllStringFunc(text, func(m string) string {
		sm := linkRE.FindStringSubmatch(m)
		linkURLs = append(linkURLs, strings.ReplaceAll(sm[2], `"`, "&quot;"))
		return `<a href="` + "\x00LK" + strconv.Itoa(len(linkURLs)-1) + "\x00" + `">` + sm[1] + "</a>"
	})

	// 7-9. Bold, italic, strikethrough.
	text = boldStarRE.ReplaceAllString(text, "<b>${1}</b>")
	text = boldUnderRE.ReplaceAllString(text, "<b>${1}</b>")
	text = italicize(text)
	text = strikeRE.ReplaceAllString(text, "<s>${1}</s>")

	// 10. Bullets, and numbered items with one space after the dot.
	text = bulletRE.ReplaceAllString(text, "• ")
	text = numberedRE.ReplaceAllString(text, "${1}. ")

	// 11-13. Restore what was parked, then the header markers.
	for i, u := range linkURLs {
		text = strings.ReplaceAll(text, "\x00LK"+strconv.Itoa(i)+"\x00", u)
	}
	for i, code := range inlineCodes {
		text = strings.ReplaceAll(text, "\x00IC"+strconv.Itoa(i)+"\x00", "<code>"+escapeHTML(code)+"</code>")
	}
	for i, code := range codeBlocks {
		text = strings.ReplaceAll(text, "\x00CB"+strconv.Itoa(i)+"\x00", "<pre><code>"+escapeHTML(code)+"</code></pre>")
	}
	text = strings.ReplaceAll(text, "⟪B⟫", "<b>")
	return strings.ReplaceAll(text, "⟪/B⟫", "</b>")
}

// prefixLen returns the byte length of the longest prefix of s within
// limit UTF-16 code units.
func prefixLen(s string, limit int) int {
	units := 0
	for i, r := range s {
		n := utf16.RuneLen(r)
		if n < 0 {
			n = 1 // invalid bytes decode as U+FFFD
		}
		if units+n > limit {
			return i
		}
		units += n
	}
	return len(s)
}

// hardCut is the byte offset of a cut at limit units with no better
// break: at least one character, so splitting always advances.
func hardCut(s string, limit int) int {
	if n := prefixLen(s, limit); n > 0 {
		return n
	}
	_, n := utf8.DecodeRuneInString(s)
	return n
}

// lastBreak returns the byte offset of the last newline in s[from:],
// else the last space, else -1.
func lastBreak(s string, from int) int {
	if i := strings.LastIndexByte(s[from:], '\n'); i >= 0 {
		return from + i
	}
	if i := strings.LastIndexByte(s[from:], ' '); i >= 0 {
		return from + i
	}
	return -1
}

// openFence returns where, in content[:pos], a fenced code block is
// still open: the byte offset of its opening line, that line and its
// fence ("" fence when none is open). Inline ``` pairs count too, as
// replaceFencedCode treats them.
func openFence(content string, pos int) (opening int, line, fence string) {
	inline := false
	offset := 0
	for _, l := range strings.Split(content[:pos], "\n") {
		if fence != "" && !inline {
			if closesCodeFence(l, fence) {
				fence = ""
			}
			offset += len(l) + 1
			continue
		}
		marker := ""
		if fence == "" {
			marker = openingCodeFence(l)
		}
		if marker != "" {
			fence, opening, inline = marker, offset, false
		} else {
			for at := 0; ; {
				k := strings.Index(l[at:], "```")
				if k < 0 {
					break
				}
				if fence == "" {
					fence, opening, inline = "```", offset+at+k, true
				} else {
					fence, inline = "", false
				}
				at += k + 3
			}
		}
		offset += len(l) + 1
	}
	if fence == "" {
		return 0, "", ""
	}
	line = content[opening:]
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return opening, line, fence
}

// splitMarkdown splits raw Markdown into chunks of at most limit UTF-16
// code units without leaving a fenced code block unbalanced, like
// nanobot's _split_telegram_markdown: a cut inside a block moves before
// its opening fence, or, for a block that starts the chunk, closes it
// and reopens it at the start of the next chunk.
func splitMarkdown(content string, limit int) []string {
	content = strings.TrimLeft(content, " \t\n\r\v\f")
	var chunks []string
	hard := func() {
		n := hardCut(content, limit)
		chunks = append(chunks, content[:n])
		content = strings.TrimLeft(content[n:], " \t\n\r\v\f")
	}
	for content != "" {
		if utf16Len(content) <= limit {
			chunks = append(chunks, content)
			break
		}
		cut := prefixLen(content, limit)
		pos := strings.LastIndexByte(content[:cut], '\n')
		if pos <= 0 {
			pos = strings.LastIndexByte(content[:cut], ' ')
		}
		if pos <= 0 {
			pos = hardCut(content, limit)
		}
		opening, line, fence := openFence(content, pos)
		if fence == "" {
			chunks = append(chunks, content[:pos])
			content = strings.TrimLeft(content[pos:], " \t\n\r\v\f")
			continue
		}
		if opening > 0 {
			chunks = append(chunks, content[:opening])
			content = strings.TrimLeft(content[opening:], " \t\n\r\v\f")
			continue
		}
		// The block opens the chunk: close it at a break and reopen it.
		closing := "\n" + fence
		minCode := len(line)
		if strings.HasPrefix(content, line+"\n") {
			minCode++
		}
		// minCode is a byte offset, budgets are UTF-16 units.
		minUnits := utf16Len(content[:minCode])
		if pos < minCode || utf16Len(content[:pos])+len(closing) > limit {
			// The only break is the opening fence's newline (cutting
			// there would never advance), or the closer would not fit.
			budget := limit - len(closing)
			if budget <= minUnits {
				hard()
				continue
			}
			recut := content[:prefixLen(content, budget)]
			if adjusted := lastBreak(recut, minCode); adjusted > minCode {
				pos = adjusted
			} else {
				pos = len(recut)
			}
		}
		if pos <= minCode {
			hard()
			continue
		}
		chunks = append(chunks, content[:pos]+closing)
		content = line + "\n" + strings.TrimPrefix(content[pos:], "\n")
	}
	return chunks
}

// htmlChunk is one outbound message: its raw Markdown (sent as plain
// text when Telegram cannot parse the HTML) and its rendering.
type htmlChunk struct{ raw, html string }

// splitMarkdownHTML splits raw Markdown into chunks whose HTML fits
// limit UTF-16 units (nanobot's _split_telegram_markdown_html_chunks).
// The Markdown is split first at maxMarkdownLen; a chunk whose HTML
// grew past limit (tags, entities) is split again with a budget scaled
// down by how much it grew, so HTML is never cut through a tag.
func splitMarkdownHTML(content string, limit int) []htmlChunk {
	var out []htmlChunk
	pending := splitMarkdown(content, min(maxMarkdownLen, limit))
	for len(pending) > 0 {
		// Telegram trims messages; trimmed here, a chunk's raw and HTML
		// forms are what arrives.
		raw := strings.TrimRight(pending[0], " \t\n\r\v\f")
		pending = pending[1:]
		if raw == "" {
			continue
		}
		html := markdownToTelegramHTML(raw)
		rawLen, htmlLen := utf16Len(raw), utf16Len(html)
		if htmlLen <= limit {
			out = append(out, htmlChunk{raw, html})
			continue
		}
		next := min(max(1, rawLen*limit/htmlLen-8), rawLen-1)
		var parts []string
		if next > 0 {
			parts = splitMarkdown(raw, next)
		}
		if len(parts) == 1 && parts[0] == raw { // cannot shrink: leave it to the plain fallback
			out = append(out, htmlChunk{raw, html})
			continue
		}
		pending = append(parts, pending...)
	}
	return out
}
