package mcp

import (
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"
)

// Tool names must fit what model providers accept: [A-Za-z0-9_-]{1,64}.
const (
	maxToolName = 64
	hashLen     = 8
)

var (
	invalidNameChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	underscoreRuns   = regexp.MustCompile(`_+`)
)

// sanitize replaces characters providers reject with "_" and collapses
// runs of "_", like nanobot's _sanitize_name.
func sanitize(s string) string {
	return underscoreRuns.ReplaceAllString(invalidNameChars.ReplaceAllString(s, "_"), "_")
}

// toolName returns the model-facing name mcp_<server>_<tool>. Names over
// 64 characters keep a prefix and end in a hash of the unsanitized name,
// so two long names that differ only after the cut stay distinct (unlike
// plain truncation).
func toolName(server, tool string) string {
	full := "mcp_" + server + "_" + tool
	name := sanitize(full)
	if len(name) <= maxToolName {
		return name
	}
	sum := sha1.Sum([]byte(full))
	prefix := strings.TrimRight(name[:maxToolName-hashLen-1], "_")
	return prefix + "_" + hex.EncodeToString(sum[:])[:hashLen]
}
