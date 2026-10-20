package cron

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/yedamao/nanobot-go/internal/tools"
)

// ToolConfig configures the cron tool.
type ToolConfig struct {
	// Location is the zone for naive at times and for cron expressions
	// without tz, and for showing times. Nil means the local zone, which
	// is stored as "no zone" so jobs follow the gateway host's zone.
	Location *time.Location
	// AddNote is appended to the result of a successful add, to tell the
	// model something about where the job will run (`nanobot agent`
	// uses it to say jobs only run under the gateway).
	AddNote string
}

// Tool is the agent's cron tool (nanobot/agent/tools/cron.py): add, list
// and remove scheduled jobs. A job added through it is bound to the chat
// of the current turn (tools.TurnFrom), so it fails outside a chat turn
// (Dream) and inside a cron turn (a job cannot schedule jobs).
type Tool struct {
	svc  *Service
	loc  *time.Location
	note string
}

// NewTool returns the cron tool for svc.
func NewTool(svc *Service, cfg ToolConfig) *Tool {
	return &Tool{svc: svc, loc: cfg.Location, note: cfg.AddNote}
}

func (t *Tool) Name() string { return "cron" }

func (t *Tool) Description() string {
	return "Schedule reminders and recurring tasks. Actions: add, list, remove. " +
		"add requires exactly one schedule: every_seconds, cron_expr, or at. " +
		fmt.Sprintf("If tz is omitted, cron expressions and naive ISO times use %s.", t.zoneName())
}

func (t *Tool) Parameters() map[string]any {
	every := tools.Integer("Interval in seconds (for recurring tasks)")
	every["minimum"] = 1
	action := tools.String("Action to perform")
	action["enum"] = []string{"add", "list", "remove"}
	return tools.Object(map[string]any{
		"action": action,
		"name":   tools.String("Job label; generated from message if omitted."),
		"message": tools.String("REQUIRED when action='add': non-empty instruction to execute when the job triggers. " +
			"The final reply goes to the task's saved chat automatically. " +
			"Do not copy the current channel/chat ID into it."),
		"every_seconds": every,
		"cron_expr":     tools.String("Cron expression like '0 9 * * *' (for scheduled tasks)"),
		"tz":            tools.String("IANA timezone for cron_expr only, e.g. 'America/Vancouver'."),
		"at": tools.String("ISO datetime for one-time execution (e.g. '2026-02-12T10:30:00'). " +
			"Naive values use the tool's default timezone."),
		"job_id": tools.String("REQUIRED when action='remove'. Job ID to remove (obtain via action='list')."),
	}, "action")
}

func (t *Tool) ReadOnly() bool { return false }

type toolArgs struct {
	Action       string `json:"action"`
	Name         string `json:"name"`
	Message      string `json:"message"`
	EverySeconds *int64 `json:"every_seconds"`
	CronExpr     string `json:"cron_expr"`
	TZ           string `json:"tz"`
	At           string `json:"at"`
	JobID        string `json:"job_id"`
}

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	args, err := tools.DecodeArgs[toolArgs](raw)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	switch args.Action {
	case "add":
		return t.add(ctx, args)
	case "list":
		return t.list()
	case "remove":
		return t.remove(args.JobID)
	}
	return tools.Errorf("unknown action %q", args.Action)
}

func (t *Tool) add(ctx context.Context, a toolArgs) tools.Result {
	turn, ok := tools.TurnFrom(ctx)
	if ok && IsCronTurn(turn.Metadata) {
		return tools.Errorf("cannot schedule new jobs from within a cron job execution")
	}
	if strings.TrimSpace(a.Message) == "" {
		return tools.Errorf("cron action='add' requires a non-empty 'message' parameter describing what to do " +
			"when the job triggers (e.g. the reminder text). Retry including message=\"...\".")
	}
	if !ok || turn.SessionKey == "" || turn.Channel == "" || turn.ChatID == "" {
		return tools.Errorf("scheduled cron jobs must be created from a chat session")
	}
	if a.TZ != "" && a.CronExpr == "" {
		return tools.Errorf("tz can only be used with cron_expr")
	}
	n := 0
	for _, set := range []bool{a.EverySeconds != nil, a.CronExpr != "", a.At != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return tools.Errorf("exactly one of every_seconds, cron_expr, or at is required")
	}

	spec := AddSpec{
		Name:    a.Name,
		Message: a.Message,
		Binding: &Binding{Channel: turn.Channel, ChatID: turn.ChatID, SessionKey: turn.SessionKey, Metadata: persistable(turn.Metadata)},
	}
	switch {
	case a.EverySeconds != nil:
		secs := *a.EverySeconds
		if secs <= 0 || secs > math.MaxInt64/int64(time.Second) {
			return tools.Errorf("every_seconds must be a positive integer")
		}
		spec.Schedule = Schedule{Kind: KindEvery, Every: time.Duration(secs) * time.Second}
	case a.CronExpr != "":
		tz := a.TZ
		if tz == "" && t.loc != nil && t.loc != time.Local {
			tz = t.loc.String()
		}
		if tz != "" {
			if _, err := LoadLocation(tz); err != nil {
				return tools.Errorf("%v", err)
			}
		}
		spec.Schedule = Schedule{Kind: KindCron, Expr: a.CronExpr, TZ: tz}
	default:
		at, err := ParseAt(a.At, t.location())
		if err != nil {
			return tools.Errorf("%v", err)
		}
		if !at.After(t.svc.clock.Now()) {
			return tools.Errorf("one-time job time '%s' is not in the future. "+
				"Retry with a future ISO datetime computed from the current time.", a.At)
		}
		spec.Schedule = Schedule{Kind: KindAt, At: at}
		spec.DeleteAfterRun = true
	}
	job, err := t.svc.Add(spec)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	out := fmt.Sprintf("Created job '%s' (id: %s)", job.Name, job.ID)
	if t.note != "" {
		out += "\n" + t.note
	}
	return tools.Text(out)
}

func (t *Tool) list() tools.Result {
	jobs, err := t.svc.List(false)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if len(jobs) == 0 {
		return tools.Text("No scheduled jobs.")
	}
	var b strings.Builder
	b.WriteString("Scheduled jobs:")
	for _, j := range jobs {
		fmt.Fprintf(&b, "\n- %s (id: %s, %s)", j.Name, j.ID, t.timing(j.Schedule))
		if j.System() {
			fmt.Fprintf(&b, "\n  Purpose: %s", systemPurpose(j))
			b.WriteString("\n  Protected: visible for inspection, but cannot be removed.")
		}
		loc := t.displayLocation(j.Schedule)
		if !j.State.LastRun.IsZero() {
			fmt.Fprintf(&b, "\n  Last run: %s — %s", FormatTime(j.State.LastRun, loc), cmp.Or(string(j.State.LastStatus), "unknown"))
			if j.State.LastError != "" {
				fmt.Fprintf(&b, " (%s)", j.State.LastError)
			}
		}
		if !j.State.NextRun.IsZero() {
			fmt.Fprintf(&b, "\n  Next run: %s", FormatTime(j.State.NextRun, loc))
		}
	}
	return tools.Text(b.String())
}

func (t *Tool) remove(id string) tools.Result {
	if strings.TrimSpace(id) == "" {
		return tools.Errorf("job_id is required for remove")
	}
	res, err := t.svc.Remove(id)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	switch res {
	case Removed:
		return tools.Text("Removed job " + id)
	case Protected:
		if id == DreamJobID {
			return tools.Text("Cannot remove job `dream`.\n" +
				"This is a system-managed Dream memory consolidation job for long-term memory.\n" +
				"It remains visible so you can inspect it, but it cannot be removed.")
		}
		return tools.Text(fmt.Sprintf("Cannot remove job `%s`.\nThis is a protected system-managed cron job.", id))
	}
	return tools.Text(fmt.Sprintf("Job %s not found", id))
}

// timing describes a schedule, showing at times in the tool's zone.
func (t *Tool) timing(s Schedule) string {
	if s.Kind == KindAt {
		return "at " + FormatTime(s.At, t.location())
	}
	return s.String()
}

func (t *Tool) location() *time.Location {
	if t.loc == nil {
		return time.Local
	}
	return t.loc
}

func (t *Tool) zoneName() string {
	if loc := t.location(); loc != time.Local {
		return loc.String()
	}
	return "the gateway host's local time zone"
}

// displayLocation picks the zone a job's times read best in: its own,
// else the tool's.
func (t *Tool) displayLocation(s Schedule) *time.Location {
	if s.TZ != "" {
		if loc, err := LoadLocation(s.TZ); err == nil {
			return loc
		}
	}
	return t.location()
}

// DreamJobID is the id (and name) of the Dream system job.
const DreamJobID = "dream"

func systemPurpose(j Job) string {
	if j.ID == DreamJobID {
		return "Dream memory consolidation for long-term memory."
	}
	return "System-managed internal job."
}

// FormatTime shows t in loc with its zone abbreviation.
func FormatTime(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02 15:04:05 MST")
}

// atLayouts are the ISO 8601 forms ParseAt accepts, like Python's
// datetime.fromisoformat.
var atLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04",
	"2006-01-02",
}

// ParseAt parses an ISO 8601 date-time. Values without an offset are in
// loc.
func ParseAt(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range atLayouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid ISO datetime format '%s'. Expected format: YYYY-MM-DDTHH:MM:SS", s)
}

// persistable keeps the metadata values that survive a JSON round trip
// and drops the cron trigger marker, like nanobot's
// _persistable_origin_metadata.
func persistable(meta map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range meta {
		if k == TriggerMetaKey {
			continue
		}
		data, err := json.Marshal(v)
		if err != nil {
			continue
		}
		var cp any
		if json.Unmarshal(data, &cp) == nil {
			out[k] = cp
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
