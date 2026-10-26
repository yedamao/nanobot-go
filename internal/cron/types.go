// Package cron schedules agent turns, porting nanobot/cron. A Job pairs a
// Schedule (once at a time, every interval, or a cron expression in a
// time zone) with a message. Jobs are persisted in
// <workspace>/cron/jobs.json by a Store, run by a Service's timer
// goroutine through an injected RunFunc, and created by the agent through
// the cron Tool. A job is bound to the chat it was created from: when it
// fires, BoundRunner runs the message as a turn in that chat's session and
// delivers the reply back to it.
package cron

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Kind is how a job is scheduled.
type Kind string

// Schedule kinds.
const (
	// KindAt runs once at Schedule.At.
	KindAt Kind = "at"
	// KindEvery runs every Schedule.Every, counted from the previous run
	// (or from when the job was added).
	KindEvery Kind = "every"
	// KindCron runs at the times matching Schedule.Expr in Schedule.TZ.
	KindCron Kind = "cron"
)

// Schedule says when a job runs.
type Schedule struct {
	Kind  Kind
	At    time.Time
	Every time.Duration
	Expr  string
	// TZ is the IANA zone for Expr; empty means the gateway's local zone.
	TZ string
}

// scheduleJSON is Schedule on disk; the interval is in milliseconds like
// nanobot's everyMs.
type scheduleJSON struct {
	Kind    Kind      `json:"kind"`
	At      time.Time `json:"at,omitzero"`
	EveryMs int64     `json:"everyMs,omitempty"`
	Expr    string    `json:"expr,omitempty"`
	TZ      string    `json:"tz,omitempty"`
}

// MarshalJSON implements json.Marshaler.
func (s Schedule) MarshalJSON() ([]byte, error) {
	return json.Marshal(scheduleJSON{Kind: s.Kind, At: s.At, EveryMs: s.Every.Milliseconds(), Expr: s.Expr, TZ: s.TZ})
}

// UnmarshalJSON implements json.Unmarshaler.
func (s *Schedule) UnmarshalJSON(b []byte) error {
	var j scheduleJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*s = Schedule{Kind: j.Kind, At: j.At, Every: time.Duration(j.EveryMs) * time.Millisecond, Expr: j.Expr, TZ: j.TZ}
	return nil
}

// Validate reports a schedule that could never run, like nanobot's
// _validate_schedule_for_add.
func (s Schedule) Validate() error {
	if s.TZ != "" && s.Kind != KindCron {
		return fmt.Errorf("tz can only be used with cron schedules")
	}
	switch s.Kind {
	case KindAt:
		if s.At.IsZero() {
			return fmt.Errorf("at schedule requires a time")
		}
	case KindEvery:
		if s.Every <= 0 {
			return fmt.Errorf("every schedule requires a positive interval")
		}
	case KindCron:
		if strings.TrimSpace(s.Expr) == "" {
			return fmt.Errorf("cron schedule requires an expression")
		}
		if _, err := ParseExpr(s.Expr); err != nil {
			return err
		}
		if _, err := s.location(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown schedule kind %q", s.Kind)
	}
	return nil
}

// Next returns the first run time after now, or false if the schedule
// has no more runs (an at time in the past, an invalid schedule).
func (s Schedule) Next(now time.Time) (time.Time, bool) {
	switch s.Kind {
	case KindAt:
		if s.At.After(now) {
			return s.At, true
		}
	case KindEvery:
		if s.Every > 0 {
			return now.Add(s.Every), true
		}
	case KindCron:
		e, err := ParseExpr(s.Expr)
		if err != nil {
			return time.Time{}, false
		}
		loc, err := s.location()
		if err != nil {
			return time.Time{}, false
		}
		if next := e.Next(now.In(loc)); !next.IsZero() {
			return next, true
		}
	}
	return time.Time{}, false
}

func (s Schedule) location() (*time.Location, error) {
	if s.TZ == "" {
		return time.Local, nil
	}
	return LoadLocation(s.TZ)
}

// LoadLocation loads an IANA time zone. The zone database is embedded
// (time/tzdata), so this works on hosts without one.
func LoadLocation(name string) (*time.Location, error) {
	// time.LoadLocation accepts "" and "UTC"/"Local"; only "Local" is
	// ambiguous across hosts, so reject it.
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("unknown timezone %q", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q", name)
	}
	return loc, nil
}

// String describes the schedule for people, like nanobot's
// _format_timing.
func (s Schedule) String() string {
	switch s.Kind {
	case KindCron:
		if s.TZ != "" {
			return fmt.Sprintf("cron: %s (%s)", s.Expr, s.TZ)
		}
		return "cron: " + s.Expr
	case KindEvery:
		ms := s.Every.Milliseconds()
		switch {
		case ms > 0 && ms%3_600_000 == 0:
			return fmt.Sprintf("every %dh", ms/3_600_000)
		case ms > 0 && ms%60_000 == 0:
			return fmt.Sprintf("every %dm", ms/60_000)
		case ms > 0 && ms%1000 == 0:
			return fmt.Sprintf("every %ds", ms/1000)
		}
		return fmt.Sprintf("every %dms", ms)
	case KindAt:
		return "at " + s.At.Format(time.RFC3339)
	}
	return string(s.Kind)
}

// PayloadKind is what a job does when it runs.
type PayloadKind string

// Payload kinds, named like nanobot's.
const (
	// PayloadAgentTurn runs Payload.Message as a turn in the bound chat.
	PayloadAgentTurn PayloadKind = "agent_turn"
	// PayloadSystem is an internal job (Dream) the gateway handles
	// itself. System jobs cannot be removed through the tool or CLI.
	PayloadSystem PayloadKind = "system_event"
)

// Payload is what a job does.
type Payload struct {
	Kind    PayloadKind `json:"kind"`
	Message string      `json:"message,omitempty"`
}

// Binding is the chat a job belongs to: its turn runs in SessionKey and
// the reply goes to Channel/ChatID with Metadata (thread ids and the
// like), as if answering a message there.
type Binding struct {
	Channel    string         `json:"channel"`
	ChatID     string         `json:"chatId"`
	SessionKey string         `json:"sessionKey"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// Complete reports whether b can route a turn and its reply.
func (b *Binding) Complete() bool {
	return b != nil && b.Channel != "" && b.ChatID != "" && b.SessionKey != ""
}

// Status is the outcome of a job's last run.
type Status string

// Run outcomes.
const (
	StatusOK      Status = "ok"
	StatusError   Status = "error"
	StatusSkipped Status = "skipped"
)

// State is a job's run bookkeeping.
type State struct {
	// NextRun is when the job is due; zero when it will not run again.
	NextRun    time.Time `json:"nextRun,omitzero"`
	LastRun    time.Time `json:"lastRun,omitzero"`
	LastStatus Status    `json:"lastStatus,omitempty"`
	LastError  string    `json:"lastError,omitempty"`
	RunCount   int       `json:"runCount,omitempty"`
	// RunHistory is the last MaxRunHistory recorded runs, oldest first,
	// like nanobot's state.run_history.
	RunHistory []RunRecord `json:"runHistory,omitempty"`
}

// MaxRunHistory caps State.RunHistory, like nanobot's _MAX_RUN_HISTORY.
const MaxRunHistory = 20

// RunRecord is one recorded run of a job, like nanobot's CronRunRecord.
type RunRecord struct {
	RunAt      time.Time `json:"runAt"`
	Status     Status    `json:"status"`
	DurationMs int64     `json:"durationMs,omitempty"`
	// Error is the truncated run error or skip reason.
	Error string `json:"error,omitempty"`
	// Reply is the agent's reply, truncated to maxReplyChars runes. Full
	// replies are not kept (no nanobot runs/ audit files).
	Reply string `json:"reply,omitempty"`
}

// Job is a scheduled task.
type Job struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Schedule Schedule `json:"schedule"`
	Payload  Payload  `json:"payload"`
	// Binding is nil for system jobs and for jobs that cannot be routed;
	// the latter are skipped when due.
	Binding   *Binding  `json:"binding,omitempty"`
	State     State     `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// DeleteAfterRun removes an at job once it has run, instead of
	// disabling it.
	DeleteAfterRun bool `json:"deleteAfterRun,omitempty"`
}

// System reports whether j is an internal, protected job.
func (j *Job) System() bool { return j.Payload.Kind == PayloadSystem }

// due reports whether j should run at now.
func (j *Job) due(now time.Time) bool {
	return j.Enabled && !j.State.NextRun.IsZero() && !now.Before(j.State.NextRun)
}
