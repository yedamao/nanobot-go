// Package heartbeat ports nanobot's heartbeat: a system cron job that
// runs the active tasks of the workspace's HEARTBEAT.md as an agent turn
// in its own session, asks the model whether the result is worth telling,
// and only then sends it to the chat the user talked in most recently.
package heartbeat

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/fsutil"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/session"
)

const (
	// File is the workspace file holding the heartbeat tasks.
	File = "HEARTBEAT.md"
	// SessionKey is the session heartbeat turns run in, like nanobot's
	// HEARTBEAT_SESSION_KEY.
	SessionKey = "heartbeat"
	// SenderID is the sender of heartbeat turns.
	SenderID = "heartbeat"
	// maxFileBytes caps how much of HEARTBEAT.md is read.
	maxFileBytes = 128 << 10
)

// preamble is nanobot's _HEARTBEAT_PREAMBLE.
const preamble = "[Your response will be delivered directly to the user's messaging app. " +
	"Output ONLY the final user-facing message. Never reference internal " +
	"files (HEARTBEAT.md, AWARENESS.md, etc.), your instructions, or your " +
	"decision process. If nothing needs reporting, respond with just " +
	"'All clear.' and nothing else.]\n\n"

// Prompt returns the user message of a heartbeat turn for the content of
// HEARTBEAT.md.
func Prompt(content string) string {
	return preamble + "You are executing periodic heartbeat tasks. Read the active tasks below, perform each one, and report what you did:\n\n" + content
}

// HasActiveTasks reports whether HEARTBEAT.md has a task line under an
// "## Active Tasks" heading, ignoring blank lines, headings and HTML
// comments, like nanobot's _heartbeat_has_active_tasks.
func HasActiveTasks(content string) bool {
	inComment, inActive := false, false
	for line := range strings.Lines(content) {
		s := strings.TrimSpace(line)
		if inComment {
			if strings.Contains(s, "-->") {
				inComment = false
			}
			continue
		}
		if s == "" || strings.HasPrefix(s, "#") {
			if strings.HasPrefix(s, "##") && !strings.HasPrefix(s, "###") {
				heading := strings.ToLower(strings.TrimSpace(strings.TrimLeft(s, "#")))
				inActive = strings.HasPrefix(heading, "active tasks")
			}
			continue
		}
		if strings.HasPrefix(s, "<!--") {
			if !strings.Contains(s[4:], "-->") {
				inComment = true
			}
			continue
		}
		if inActive {
			return true
		}
	}
	return false
}

// PickTarget picks the chat heartbeat results go to, like nanobot's
// _pick_heartbeat_target_from_sessions: the most recently updated session
// (sessions come most recent first) whose key is channel:chat for a
// channel in enabled, other than cli and system. A nil enabled allows
// every channel. Keys with more parts (a Telegram topic's
// telegram:chat:topic:n) are skipped: their chat id alone does not route
// back to them.
func PickTarget(sessions []session.Info, enabled []string) (channel, chatID string, ok bool) {
	for _, s := range sessions {
		channel, chatID, found := strings.Cut(s.Key, ":")
		switch {
		case !found || chatID == "" || strings.Contains(chatID, ":"):
		case channel == "cli" || channel == "system":
		case enabled != nil && !slices.Contains(enabled, channel):
		default:
			return channel, chatID, true
		}
	}
	return "", "", false
}

// Config wires a Runner.
type Config struct {
	// Workspace is the directory holding HEARTBEAT.md and
	// prompts/evaluator.md.
	Workspace string
	// Provider and Model run the notification check; an empty Model is the
	// provider's default.
	Provider providers.Provider
	Model    string
	// Sessions lists the stored sessions, most recently updated first
	// (session.Manager.List).
	Sessions func() ([]session.Info, error)
	// Channels returns the names of the channels messages can be sent
	// through. Nil allows every channel.
	Channels func() []string
	// Turn runs an agent turn and returns its reply without sending it
	// (agent.Loop.ProcessMessage).
	Turn func(ctx context.Context, msg bus.InboundMessage) (string, error)
	// Record, when set, adds a delivered result to the history of the
	// chat's session (agent.Loop.Record).
	Record func(ctx context.Context, sessionKey string, msgs ...providers.Message) error
	// Deliver sends a result to its chat.
	Deliver func(ctx context.Context, msg bus.OutboundMessage) error
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Runner runs the heartbeat job.
type Runner struct {
	cfg Config
	log *slog.Logger
}

// New returns a Runner.
func New(cfg Config) *Runner {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{cfg: cfg, log: log}
}

// Result is what one heartbeat run did.
type Result struct {
	// Skipped says why nothing ran: no tasks, or no chat to tell.
	Skipped string
	// Response is the turn's reply, delivered only when Notified.
	Response string
	Notified bool
}

// Run runs the heartbeat once, like nanobot's heartbeat job: with no
// active task in HEARTBEAT.md, or no chat to tell, it does nothing (and
// calls no model). Otherwise the tasks run as a turn in the heartbeat
// session with the chat's channel and id, and the reply goes to the chat
// only if the notification check says so; the check fails closed. A
// failed turn, check or delivery is not retried.
func (r *Runner) Run(ctx context.Context) (Result, error) {
	data, err := fsutil.ReadRegularFile(filepath.Join(r.cfg.Workspace, File), maxFileBytes)
	if errors.Is(err, fs.ErrNotExist) {
		r.log.Debug("heartbeat: no " + File)
		return Result{Skipped: File + " missing"}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", File, err)
	}
	content := string(data)
	if !HasActiveTasks(content) {
		r.log.Debug("heartbeat: no active tasks")
		return Result{Skipped: "no active tasks"}, nil
	}

	sessions, err := r.cfg.Sessions()
	if err != nil {
		return Result{}, fmt.Errorf("list sessions: %w", err)
	}
	var enabled []string
	if r.cfg.Channels != nil {
		enabled = r.cfg.Channels()
		if enabled == nil {
			enabled = []string{}
		}
	}
	channel, chatID, ok := PickTarget(sessions, enabled)
	if !ok {
		r.log.Debug("heartbeat: no chat to report to")
		return Result{Skipped: "no chat to report to"}, nil
	}

	prompt := Prompt(content)
	reply, err := r.cfg.Turn(ctx, bus.InboundMessage{
		Channel:            channel,
		ChatID:             chatID,
		SenderID:           SenderID,
		Content:            prompt,
		Timestamp:          time.Now(),
		SessionKeyOverride: SessionKey,
	})
	if err != nil {
		return Result{}, err
	}
	res := Result{Response: reply}
	if strings.TrimSpace(reply) == "" {
		return res, nil
	}
	if !Evaluate(ctx, r.cfg.Provider, r.cfg.Model, EvaluatorPrompt(r.cfg.Workspace, r.log), prompt, reply, r.log) {
		r.log.Info("heartbeat: silenced by post-run evaluation")
		return res, nil
	}
	r.log.Info("heartbeat: delivering response", "channel", channel, "chat_id", chatID)
	if err := r.cfg.Deliver(ctx, bus.OutboundMessage{Channel: channel, ChatID: chatID, Content: reply}); err != nil {
		return res, fmt.Errorf("deliver: %w", err)
	}
	res.Notified = true
	if r.cfg.Record != nil {
		msg := providers.Message{Role: providers.RoleAssistant, Content: reply}
		if err := r.cfg.Record(ctx, channel+":"+chatID, msg); err != nil {
			r.log.Warn("heartbeat: record delivery", "error", err)
		}
	}
	return res, nil
}
