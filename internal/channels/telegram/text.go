package telegram

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxMessageLen is Telegram's limit for one message's text. Telegram
// counts UTF-16 code units, so a character outside the Basic
// Multilingual Plane (most emoji) counts twice.
const maxMessageLen = 4096

// utf16Len returns the length of s in UTF-16 code units.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r) // invalid bytes decode as U+FFFD, one unit
	}
	return n
}

// splitMessage splits text into chunks of at most limit UTF-16 code units,
// preferring to break after a newline, then after a space, and otherwise
// between characters. Spaces and newlines around chunks are dropped
// (Telegram trims them anyway); text that is only whitespace yields no
// chunks.
func splitMessage(text string, limit int) []string {
	text = strings.Trim(text, " \n")
	var chunks []string
	for text != "" {
		if utf16Len(text) <= limit {
			chunks = append(chunks, text)
			break
		}
		// end is the byte offset where limit units are reached.
		end, units := 0, 0
		for i, r := range text {
			n := utf16.RuneLen(r)
			if units+n > limit {
				break
			}
			units += n
			_, size := utf8.DecodeRuneInString(text[i:])
			end = i + size
		}
		if end == 0 { // limit is below one character: never loop forever
			_, end = utf8.DecodeRuneInString(text)
		}
		cut := end
		if i := strings.LastIndexByte(text[:end], '\n'); i > 0 {
			cut = i + 1
		} else if i := strings.LastIndexByte(text[:end], ' '); i > 0 {
			cut = i + 1
		}
		chunks = append(chunks, strings.TrimRight(text[:cut], " \n"))
		text = strings.TrimLeft(text[cut:], " \n")
	}
	return chunks
}
