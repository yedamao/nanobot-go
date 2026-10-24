package agent

import (
	"context"
	"encoding/json"
	"maps"
	"strings"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// hintMaxLen caps the argument part of a tool hint, in characters.
const hintMaxLen = 40

// hintFormat describes how ToolHint shows one tool: the arguments to try
// in order, the text around the value, and how to shorten it.
type hintFormat struct {
	keys   []string
	prefix string
	suffix string
	path   bool // keep the end (the file name) rather than the start
}

// hintFormats covers the built-in tools, after nanobot's
// utils/tool_hints.py.
var hintFormats = map[string]hintFormat{
	"read_file":  {keys: []string{"path", "file_path"}, prefix: "read ", path: true},
	"write_file": {keys: []string{"path", "file_path"}, prefix: "write ", path: true},
	"edit_file":  {keys: []string{"path", "file_path"}, prefix: "edit ", path: true},
	"list_dir":   {keys: []string{"path"}, prefix: "ls ", path: true},
	"exec":       {keys: []string{"command"}, prefix: "$ "},
	"web_fetch":  {keys: []string{"url"}, prefix: "fetch "},
	"web_search": {keys: []string{"query"}, prefix: `search "`, suffix: `"`},
}

// ToolHint returns a short, human-readable description of call, such as
// `read …/notes/todo.md`, `$ go test ./...` or `cron("list")`, for a
// status line. It never fails: malformed arguments yield the bare name.
func ToolHint(call providers.ToolCall) string {
	var args map[string]any
	_ = json.Unmarshal(call.Arguments, &args)
	if f, ok := hintFormats[call.Name]; ok {
		v := firstString(args, f.keys)
		if v == "" {
			return call.Name
		}
		if f.path {
			v = shortenPath(v, hintMaxLen)
		} else {
			v = shorten(v, hintMaxLen)
		}
		return f.prefix + v + f.suffix
	}
	name := call.Name
	if rest, ok := strings.CutPrefix(name, "mcp_"); ok {
		if server, tool, ok := strings.Cut(rest, "__"); ok && tool != "" {
			name = server + "::" + tool
		}
	}
	if v := firstString(args, nil); v != "" {
		return name + `("` + shorten(v, hintMaxLen) + `")`
	}
	return name
}

// firstString returns the first non-empty string among args[keys], or,
// when none is, any non-empty string argument (the smallest key, so the
// choice is stable).
func firstString(args map[string]any, keys []string) string {
	for _, k := range keys {
		if s, _ := args[k].(string); s != "" {
			return oneLine(s)
		}
	}
	best, bestKey := "", ""
	for k, v := range args {
		if s, _ := v.(string); s != "" && (bestKey == "" || k < bestKey) {
			best, bestKey = s, k
		}
	}
	return oneLine(best)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// shorten cuts s to n characters, ending in "…" when cut.
func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// shortenPath cuts a path to n characters by dropping leading
// directories ("…/dir/file"); a file name that is too long on its own is
// cut at the end.
func shortenPath(p string, n int) string {
	if len([]rune(p)) <= n {
		return p
	}
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	base := parts[len(parts)-1]
	if len(parts) == 1 || len([]rune(base))+2 > n {
		return shorten(base, n)
	}
	out := base
	for i := len(parts) - 2; i >= 0; i-- {
		next := parts[i] + "/" + out
		if len([]rune(next))+2 > n {
			break
		}
		out = next
	}
	return "…/" + out
}

// progressHooks publishes a progress update (see bus.IsProgress) to the
// chat a turn came from as each tool call starts.
type progressHooks struct {
	NopHooks
	bus     *bus.MessageBus
	channel string
	chatID  string
	meta    map[string]any
}

// newProgressHooks returns hooks that report msg's turn back to its chat.
func newProgressHooks(b *bus.MessageBus, msg bus.InboundMessage) *progressHooks {
	return &progressHooks{bus: b, channel: msg.Channel, chatID: msg.ChatID, meta: msg.Metadata}
}

// OnToolStart publishes the call's hint. It is safe for concurrent use:
// every update gets its own Metadata map. A failed publish (the turn is
// ending) is ignored.
func (h *progressHooks) OnToolStart(ctx context.Context, call providers.ToolCall) {
	meta := maps.Clone(h.meta)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[bus.MetaProgress] = true
	_ = h.bus.PublishOutbound(ctx, bus.OutboundMessage{Channel: h.channel, ChatID: h.chatID, Content: ToolHint(call), Metadata: meta})
}

// hookList calls each of its hooks in order.
type hookList []Hooks

// joinHooks returns hooks that call each non-nil hook in hs, or nil when
// there is none.
func joinHooks(hs ...Hooks) Hooks {
	var list hookList
	for _, h := range hs {
		if h != nil {
			list = append(list, h)
		}
	}
	switch len(list) {
	case 0:
		return nil
	case 1:
		return list[0]
	}
	return list
}

func (l hookList) BeforeIteration(ctx context.Context, iteration int, messages []providers.Message) {
	for _, h := range l {
		h.BeforeIteration(ctx, iteration, messages)
	}
}

func (l hookList) OnToolStart(ctx context.Context, call providers.ToolCall) {
	for _, h := range l {
		h.OnToolStart(ctx, call)
	}
}

func (l hookList) OnToolEnd(ctx context.Context, call providers.ToolCall, result tools.Result) {
	for _, h := range l {
		h.OnToolEnd(ctx, call, result)
	}
}

func (l hookList) OnContent(ctx context.Context, content string) {
	for _, h := range l {
		h.OnContent(ctx, content)
	}
}
