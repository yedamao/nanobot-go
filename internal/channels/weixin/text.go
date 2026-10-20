package weixin

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// maxMessageLen is the longest text sent in one WeChat message, in
// characters (nanobot: WEIXIN_MAX_MESSAGE_LEN).
const maxMessageLen = 1800

var (
	// codeRE matches fenced and inline code, which is kept byte for byte.
	codeRE = regexp.MustCompile("(```[\\s\\S]*?```|`[^`\\n]*`)")
	// imageRE matches a Markdown image, which WeChat cannot show.
	imageRE = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	// deepHeadingRE matches the markers of level 5 and 6 headings.
	deepHeadingRE = regexp.MustCompile(`(?m)^#{5,6}\s+`)
	// markupReplacer drops strikethrough markers and swaps angle brackets
	// for their full-width forms: the WeChat client treats a bare "<" as
	// markup and may hide the rest of the message.
	markupReplacer = strings.NewReplacer("<", "＜", ">", "＞", "~~", "")
)

// sanitizeMarkdown removes what renders badly in the WeChat client
// (nanobot: sanitize_weixin_markdown): images, strikethrough markers,
// level 5-6 heading markers and bare angle brackets, outside code.
func sanitizeMarkdown(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	last := 0
	clean := func(text string) {
		text = imageRE.ReplaceAllString(text, "")
		text = markupReplacer.Replace(text)
		b.WriteString(deepHeadingRE.ReplaceAllString(text, ""))
	}
	for _, loc := range codeRE.FindAllStringIndex(s, -1) {
		clean(s[last:loc[0]])
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	clean(s[last:])
	return b.String()
}

// splitMessage sanitizes text and splits it into messages of at most
// limit characters (nanobot: split_weixin_message). It breaks at a blank
// line, else a newline, else after sentence punctuation or a space, and
// keeps code fences balanced: a chunk that leaves a fence open closes it
// and the next one reopens it.
func splitMessage(text string, limit int) []string {
	text = strings.TrimSpace(sanitizeMarkdown(text))
	if text == "" {
		return nil
	}
	rest := []rune(text)
	if limit <= 0 || len(rest) <= limit {
		return []string{text}
	}
	var chunks []string
	inFence := false
	for len(rest) > 0 {
		prefix := ""
		if inFence {
			prefix = "```\n"
		}
		// Leave room for the "\n```" that closes a fence left open.
		avail := limit - len([]rune(prefix)) - 4
		if avail <= 0 {
			return []string{text}
		}
		piece := rest
		if len(rest) > avail {
			cand := rest[:avail]
			cut := lastIndex(cand, "\n\n")
			if cut <= 0 {
				cut = lastIndex(cand, "\n")
			}
			if cut <= 0 {
				p := -1
				for _, mark := range "。！？；.!?; " {
					p = max(p, lastIndex(cand, string(mark)))
				}
				cut = avail
				if p >= 0 {
					cut = p + 1
				}
			}
			piece = rest[:cut]
		}
		rest = trimLeftSpace(rest[len(piece):])
		raw := string(piece)
		next := inFence != (strings.Count(raw, "```")%2 == 1)
		chunk := prefix + strings.TrimRightFunc(raw, unicode.IsSpace)
		if next {
			chunk += "\n```"
		}
		chunks = append(chunks, chunk)
		inFence = next
	}
	return chunks
}

// lastIndex returns the rune index of the last sub in rs, or -1.
func lastIndex(rs []rune, sub string) int {
	sr := []rune(sub)
	for i := len(rs) - len(sr); i >= 0; i-- {
		if slices.Equal(rs[i:i+len(sr)], sr) {
			return i
		}
	}
	return -1
}

func trimLeftSpace(rs []rune) []rune {
	for len(rs) > 0 && unicode.IsSpace(rs[0]) {
		rs = rs[1:]
	}
	return rs
}
