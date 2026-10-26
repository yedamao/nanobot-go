// Package message is the agent's message tool (nanobot/agent/tools/message.py):
// it sends a separate chat message, optionally with file attachments and
// choice buttons, through the bus. Channels deliver OutboundMessage.Media as they would a
// reply's (Telegram uploads photos and documents, WeChat uploads them to
// its CDN, the WebUI shows media frames). Buttons become a Telegram inline
// keyboard when channels.telegram.inlineKeyboards is on, and text such as
// "[Yes] [No]" under the content everywhere else.
package message

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/heartbeat"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// MaxMedia caps the attachments of one message, like Telegram's limit on
// the files of one reply.
const MaxMedia = 10

// Tool sends messages to chats. Without a target in the arguments it
// sends to the chat of the current turn (tools.TurnFrom) and keeps the
// turn's routing metadata (threads, the message replied to); a message
// to another chat gets none, so it cannot be routed back into this one.
//
// Sends from a heartbeat turn are acknowledged but not delivered: the
// heartbeat's reply only reaches the chat after its evaluator approves
// it, and the tool must not bypass that.
type Tool struct {
	bus *bus.MessageBus
	ws  *security.Workspace
}

// NewTool returns the message tool publishing to b, with attachments
// resolved in ws. A nil b makes every send fail.
func NewTool(b *bus.MessageBus, ws *security.Workspace) *Tool {
	return &Tool{bus: b, ws: ws}
}

func (t *Tool) Name() string { return "message" }

func (t *Tool) Description() string {
	return "Send separate messages requested by the user or task, optionally with attachments. " +
		"For normal replies, including scheduled reminders and reports, answer directly; " +
		"the final reply is delivered to the current chat automatically. " +
		"Use this tool to deliver files or images from the workspace via media. " +
		"Optional buttons offer choices as an inline keyboard (rows of labels); " +
		"a tapped button comes back as a message with its label."
}

func (t *Tool) Parameters() map[string]any {
	media := map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": fmt.Sprintf("Optional list of existing workspace file paths to attach (at most %d).", MaxMedia),
	}
	buttons := map[string]any{
		"type": "array",
		"items": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string", "description": "Button label"},
		},
		"description": "Optional: inline keyboard buttons as list of rows, each row is list of button labels.",
	}
	return tools.Object(map[string]any{
		"content": tools.String("Message content."),
		"channel": tools.String("Target channel; defaults to the current channel."),
		"chat_id": tools.String("Target chat/user ID; defaults to the current chat."),
		"media":   media,
		"buttons": buttons,
	}, "content")
}

func (t *Tool) ReadOnly() bool { return false }

type args struct {
	Content string   `json:"content"`
	Channel string   `json:"channel"`
	ChatID  string   `json:"chat_id"`
	Media   []string `json:"media"`
	Buttons any      `json:"buttons"`
}

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	a, err := tools.DecodeArgs[args](raw)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	turn, _ := tools.TurnFrom(ctx)
	channel := orDefault(a.Channel, turn.Channel)
	chatID := orDefault(a.ChatID, turn.ChatID)
	if channel == "" || chatID == "" {
		return tools.Errorf("No target channel/chat specified")
	}
	if t.bus == nil {
		return tools.Errorf("Message sending not configured")
	}
	buttons, ok := buttonRows(a.Buttons)
	if !ok {
		return tools.Errorf("buttons must be a list of list of strings")
	}
	media, err := t.resolveMedia(a.Media)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	var meta map[string]any
	if channel == turn.Channel && chatID == turn.ChatID {
		meta = maps.Clone(turn.Metadata)
	}
	target := channel + ":" + chatID
	if turn.SessionKey == heartbeat.SessionKey {
		return tools.Text(fmt.Sprintf("Message acknowledged for %s (not delivered)", target))
	}
	out := bus.OutboundMessage{Channel: channel, ChatID: chatID, Content: a.Content, Media: media, Buttons: buttons, Metadata: meta}
	if err := t.bus.PublishOutbound(ctx, out); err != nil {
		return tools.Errorf("sending message: %v", err)
	}
	tools.RecordDelivery(ctx, channel, chatID)
	res := "Message sent to " + target
	if len(media) > 0 {
		res += fmt.Sprintf(" with %d attachments", len(media))
	}
	if n := countButtons(buttons); n > 0 {
		res += fmt.Sprintf(" with %d button(s)", n)
	}
	return tools.Text(res)
}

// buttonRows checks the buttons argument: absent (nil) or a list of
// lists of strings, like nanobot's check.
func buttonRows(v any) ([][]string, bool) {
	if v == nil {
		return nil, true
	}
	rows, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		labels, ok := r.([]any)
		if !ok {
			return nil, false
		}
		row := make([]string, 0, len(labels))
		for _, l := range labels {
			s, ok := l.(string)
			if !ok {
				return nil, false
			}
			row = append(row, s)
		}
		out = append(out, row)
	}
	if countButtons(out) == 0 {
		return nil, true
	}
	return out, true
}

func countButtons(rows [][]string) int {
	n := 0
	for _, r := range rows {
		n += len(r)
	}
	return n
}

// resolveMedia resolves attachment paths to existing files in the
// workspace, whether or not the workspace restricts the other tools:
// an attachment leaves the machine.
func (t *Tool) resolveMedia(paths []string) ([]string, error) {
	if len(paths) > MaxMedia {
		return nil, fmt.Errorf("too many attachments: %d (at most %d)", len(paths), MaxMedia)
	}
	var out []string
	for i, p := range paths {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
			return nil, fmt.Errorf("media[%d] is empty", i)
		case strings.HasPrefix(p, "http://"), strings.HasPrefix(p, "https://"):
			return nil, fmt.Errorf("media path is not allowed: %s: remote URLs are not supported; save the file to the workspace first", p)
		}
		abs, err := t.ws.ResolveRead(p)
		if err == nil && !t.ws.Contains(abs) {
			err = security.ErrOutsideWorkspace
		}
		if err != nil {
			return nil, fmt.Errorf("media path is not allowed: %s: %w", p, err)
		}
		info, err := os.Stat(abs)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("media file not found: %s", p)
		case err != nil:
			return nil, fmt.Errorf("media file %s: %w", p, err)
		case !info.Mode().IsRegular():
			return nil, fmt.Errorf("media path is not a file: %s", p)
		}
		out = append(out, abs)
	}
	return out, nil
}

func orDefault(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}
