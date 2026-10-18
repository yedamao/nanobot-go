// Package session stores conversations as JSONL files, one per session
// key. It ports the parts of nanobot/session/manager.py the agent loop
// needs: Session, a history window that never starts mid-turn, a
// consolidation marker with its summary, and a Manager that caches
// sessions and writes them atomically.
package session

import (
	"fmt"
	"slices"
	"time"

	"github.com/yedamao/nanobot-go/internal/providers"
)

// Session is one conversation. It is not safe for concurrent use; the
// agent loop serializes turns per session key.
type Session struct {
	Key       string
	Messages  []providers.Message
	CreatedAt time.Time
	UpdatedAt time.Time
	Metadata  map[string]any
	// LastConsolidated is the index of the first message not covered by
	// Summary (nanobot's last_consolidated). Earlier messages stay in the
	// transcript but are no longer replayed; the model sees Summary
	// instead.
	LastConsolidated int
	// Summary is the latest consolidation checkpoint. It replaces, rather
	// than extends, the previous one: each consolidation merges the old
	// summary with the newly archived messages.
	Summary string
}

// New returns an empty session.
func New(key string) *Session {
	now := time.Now()
	return &Session{Key: key, CreatedAt: now, UpdatedAt: now, Metadata: map[string]any{}}
}

// Append adds messages and bumps UpdatedAt.
func (s *Session) Append(msgs ...providers.Message) {
	s.Messages = append(s.Messages, msgs...)
	s.UpdatedAt = time.Now()
}

// Clear drops all messages and the consolidation state, starting a fresh
// conversation under the same key.
func (s *Session) Clear() {
	s.Messages = nil
	s.LastConsolidated = 0
	s.Summary = ""
	s.UpdatedAt = time.Now()
}

// Unconsolidated returns the messages after the consolidation marker. The
// result aliases the session; callers must not modify it.
func (s *Session) Unconsolidated() []providers.Message {
	return s.Messages[s.LastConsolidated:]
}

// CommitSummary records that summary covers every message before boundary
// and moves the marker there, like nanobot's commit_summary_checkpoint.
// The transcript itself is kept. Call it only after the summary has been
// stored durably elsewhere; the marker never moves backwards.
func (s *Session) CommitSummary(summary string, boundary int) error {
	if boundary < s.LastConsolidated || boundary > len(s.Messages) {
		return fmt.Errorf("commit summary: boundary %d outside [%d, %d]", boundary, s.LastConsolidated, len(s.Messages))
	}
	s.Summary = summary
	s.LastConsolidated = boundary
	s.UpdatedAt = time.Now()
	return nil
}

// History returns the most recent unconsolidated messages to replay to the
// model, at most maxMessages of them (all when maxMessages <= 0). Messages
// before the consolidation marker are never replayed; Summary stands in
// for them. Like nanobot's get_history it avoids starting mid-turn: the
// window starts at its first user message when there is one, and tool
// results whose assistant call fell outside the window are dropped. The
// result is a copy.
func (s *Session) History(maxMessages int) []providers.Message {
	msgs := s.Unconsolidated()
	if maxMessages > 0 && len(msgs) > maxMessages {
		msgs = msgs[len(msgs)-maxMessages:]
	}
	if i := slices.IndexFunc(msgs, func(m providers.Message) bool { return m.Role == providers.RoleUser }); i > 0 {
		msgs = msgs[i:]
	}
	msgs = msgs[legalStart(msgs):]
	return slices.Clone(msgs)
}

// legalStart returns the first index from which every tool result has a
// matching earlier assistant tool call, porting find_legal_message_start.
func legalStart(msgs []providers.Message) int {
	declared := map[string]bool{}
	start := 0
	for i, m := range msgs {
		switch m.Role {
		case providers.RoleAssistant:
			for _, c := range m.ToolCalls {
				if c.ID != "" {
					declared[c.ID] = true
				}
			}
		case providers.RoleTool:
			if m.ToolCallID != "" && !declared[m.ToolCallID] {
				start = i + 1
				clear(declared)
			}
		}
	}
	return start
}
