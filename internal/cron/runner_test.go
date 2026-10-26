package cron

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/bus"
)

func TestBoundRunner(t *testing.T) {
	job := Job{
		ID: "j1", Name: "standup",
		Payload: Payload{Kind: PayloadAgentTurn, Message: "post the standup"},
		Binding: &Binding{Channel: "telegram", ChatID: "42", SessionKey: "telegram:42:topic", Metadata: map[string]any{"thread_id": "7"}},
	}
	var turns []bus.InboundMessage
	var delivered []bus.OutboundMessage
	reply, turnErr := "standup time!", error(nil)
	run := BoundRunner(
		func(_ context.Context, msg bus.InboundMessage) (string, error) {
			turns = append(turns, msg)
			return reply, turnErr
		},
		func(_ context.Context, msg bus.OutboundMessage) error {
			delivered = append(delivered, msg)
			return nil
		},
	)

	if got, err := run(context.Background(), job); err != nil || got != "standup time!" {
		t.Fatalf("run = %q, %v", got, err)
	}
	if len(turns) != 1 || len(delivered) != 1 {
		t.Fatalf("turns = %v, delivered = %v", turns, delivered)
	}
	in := turns[0]
	if in.Channel != "telegram" || in.ChatID != "42" || in.SessionKey() != "telegram:42:topic" || in.SenderID != "cron" ||
		!strings.HasSuffix(in.Content, "Cron job: post the standup") || !IsCronTurn(in.Metadata) || in.Metadata["thread_id"] != "7" {
		t.Errorf("turn message = %+v", in)
	}
	out := delivered[0]
	if out.Channel != "telegram" || out.ChatID != "42" || out.Content != "standup time!" || out.Metadata["thread_id"] != "7" || IsCronTurn(out.Metadata) {
		t.Errorf("delivered = %+v", out)
	}
	// The binding's metadata is not modified.
	if IsCronTurn(job.Binding.Metadata) {
		t.Error("binding metadata gained the trigger marker")
	}

	// An empty reply delivers nothing; a failed turn neither.
	reply = "  "
	if _, err := run(context.Background(), job); err != nil || len(delivered) != 1 {
		t.Errorf("empty reply: err %v, delivered %d", err, len(delivered))
	}
	turnErr = errors.New("provider down")
	if _, err := run(context.Background(), job); !errors.Is(err, turnErr) || len(delivered) != 1 {
		t.Errorf("failed turn: err %v, delivered %d", err, len(delivered))
	}

	// Unbound and system jobs are skipped without a turn.
	n := len(turns)
	for _, j := range []Job{
		{ID: "u", Payload: Payload{Kind: PayloadAgentTurn, Message: "m"}},
		{ID: "p", Payload: Payload{Kind: PayloadAgentTurn, Message: "m"}, Binding: &Binding{Channel: "telegram", SessionKey: "telegram:"}},
		{ID: DreamJobID, Payload: Payload{Kind: PayloadSystem}},
	} {
		if got, err := run(context.Background(), j); !errors.Is(err, ErrSkipped) || got != "" {
			t.Errorf("run %s = %q, %v, want ErrSkipped", j.ID, got, err)
		}
	}
	if len(turns) != n {
		t.Error("skipped jobs ran turns")
	}
}
