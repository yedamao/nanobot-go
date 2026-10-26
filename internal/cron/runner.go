package cron

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
)

// TriggerMetaKey marks the inbound message of a cron turn (nanobot:
// _cron_trigger). Its value is a map with the job's id and name.
const TriggerMetaKey = "_cron_trigger"

// SenderID is the sender of cron turns.
const SenderID = "cron"

// reminderPrompt wraps a job's message for its turn, like nanobot's
// templates/agent/cron_reminder.md.
const reminderPrompt = `The scheduled time has arrived. Execute this scheduled cron job now and report the result to the user in the same session.

Rules:
- Return the result as a normal reply. nanobot delivers it to this task's saved chat.
- Speak directly to the user in their language.
- Do not narrate internal progress.
- Do not include user IDs.
- Do not add status reports like "Done" or "Reminded" unless they are the natural response.

Cron job: `

// Prompt returns the user message a job's turn runs with.
func Prompt(message string) string { return reminderPrompt + message }

// IsCronTurn reports whether metadata belongs to a cron turn.
func IsCronTurn(metadata map[string]any) bool {
	_, ok := metadata[TriggerMetaKey]
	return ok
}

// TurnFunc runs one agent turn for msg and returns the reply and whether a
// tool (the message tool) already sent to msg's chat during the turn
// (agent.Loop.Process's Reply.Content and Reply.Delivered).
type TurnFunc func(ctx context.Context, msg bus.InboundMessage) (reply string, delivered bool, err error)

// DeliverFunc sends a reply to a chat (bus.MessageBus.PublishOutbound in
// the gateway).
type DeliverFunc func(ctx context.Context, msg bus.OutboundMessage) error

// BoundRunner returns a RunFunc for agent-turn jobs, porting nanobot's
// run_bound_cron_job: the job's message runs as a turn in the session of
// the chat it was created from, marked as a cron turn (sender "cron",
// TriggerMetaKey in the metadata), and a non-empty reply is delivered to
// that chat and returned for the run history. When the message tool
// already sent to that chat during the turn, the reply is only returned
// (nanobot's capture_message_deliveries; the bus counterpart is
// bus.MetaSuperseded). Jobs without a complete binding, and system jobs, are
// skipped (ErrSkipped). A failed turn delivers nothing; the failure is
// recorded on the job.
func BoundRunner(turn TurnFunc, deliver DeliverFunc) RunFunc {
	return func(ctx context.Context, job Job) (string, error) {
		if job.System() {
			return "", fmt.Errorf("%w: system job %s has no handler", ErrSkipped, job.ID)
		}
		b := job.Binding
		if !b.Complete() {
			return "", fmt.Errorf("%w: job has no chat to run in; recreate it from a chat session", ErrSkipped)
		}
		meta := maps.Clone(b.Metadata)
		if meta == nil {
			meta = map[string]any{}
		}
		meta[TriggerMetaKey] = map[string]any{"job_id": job.ID, "job_name": job.Name}
		reply, delivered, err := turn(ctx, bus.InboundMessage{
			Channel:            b.Channel,
			ChatID:             b.ChatID,
			SenderID:           SenderID,
			Content:            Prompt(job.Payload.Message),
			Timestamp:          time.Now(),
			Metadata:           meta,
			SessionKeyOverride: b.SessionKey,
		})
		if err != nil {
			return reply, err
		}
		if delivered || strings.TrimSpace(reply) == "" {
			return reply, nil
		}
		return reply, deliver(ctx, bus.OutboundMessage{
			Channel:  b.Channel,
			ChatID:   b.ChatID,
			Content:  reply,
			Metadata: maps.Clone(b.Metadata),
		})
	}
}
