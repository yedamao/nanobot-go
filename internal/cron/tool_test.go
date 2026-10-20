package cron

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/tools"
)

func newToolHarness(t *testing.T, cfg ToolConfig) (*Service, *tools.Registry, *fakeClock) {
	t.Helper()
	clock := newFakeClock(t0)
	svc, err := NewService(ServiceConfig{Store: NewStore(t.TempDir()), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(NewTool(svc, cfg)); err != nil {
		t.Fatal(err)
	}
	return svc, reg, clock
}

var chatTurn = tools.Turn{Channel: "telegram", ChatID: "42", SessionKey: "telegram:42:topic",
	Metadata: map[string]any{"thread_id": 7, "reply_to": "m1", "bad": func() {}}}

func call(t *testing.T, reg *tools.Registry, ctx context.Context, args string) tools.Result {
	t.Helper()
	return reg.Execute(ctx, "cron", json.RawMessage(args))
}

func TestToolAddBindsToTheTurn(t *testing.T) {
	svc, reg, _ := newToolHarness(t, ToolConfig{Location: time.UTC})
	ctx := tools.WithTurn(context.Background(), chatTurn)

	res := call(t, reg, ctx, `{"action":"add","message":"drink water","every_seconds":"3600","name":"water"}`)
	if res.IsError || !strings.HasPrefix(res.Content, "Created job 'water' (id: ") {
		t.Fatalf("add = %+v", res)
	}
	jobs, _ := svc.List(true)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	j := jobs[0]
	b := j.Binding
	if b == nil || b.Channel != "telegram" || b.ChatID != "42" || b.SessionKey != "telegram:42:topic" {
		t.Errorf("binding = %+v", b)
	}
	// Metadata is kept as JSON would carry it; unencodable values go.
	if b.Metadata["thread_id"] != float64(7) || b.Metadata["reply_to"] != "m1" || len(b.Metadata) != 2 {
		t.Errorf("binding metadata = %v", b.Metadata)
	}
	if j.Schedule != (Schedule{Kind: KindEvery, Every: time.Hour}) || j.Payload.Message != "drink water" || j.DeleteAfterRun {
		t.Errorf("job = %+v", j)
	}
}

func TestToolAddSchedules(t *testing.T) {
	berlin := mustLoc(t, "Europe/Berlin")
	ctx := tools.WithTurn(context.Background(), chatTurn)
	tests := []struct {
		name    string
		loc     *time.Location
		args    string
		want    Schedule
		deletes bool
	}{
		{"cron with tz", nil, `{"action":"add","message":"m","cron_expr":"0 9 * * 1-5","tz":"Asia/Tokyo"}`,
			Schedule{Kind: KindCron, Expr: "0 9 * * 1-5", TZ: "Asia/Tokyo"}, false},
		{"cron default zone", berlin, `{"action":"add","message":"m","cron_expr":"0 9 * * *"}`,
			Schedule{Kind: KindCron, Expr: "0 9 * * *", TZ: "Europe/Berlin"}, false},
		{"cron local zone", nil, `{"action":"add","message":"m","cron_expr":"0 9 * * *"}`,
			Schedule{Kind: KindCron, Expr: "0 9 * * *"}, false},
		{"at naive", berlin, `{"action":"add","message":"m","at":"2026-10-21T10:30:00"}`,
			Schedule{Kind: KindAt, At: time.Date(2026, 10, 21, 8, 30, 0, 0, time.UTC)}, true},
		{"at offset", berlin, `{"action":"add","message":"m","at":"2026-10-21T10:30:00+09:00"}`,
			Schedule{Kind: KindAt, At: time.Date(2026, 10, 21, 1, 30, 0, 0, time.UTC)}, true},
		{"at space", time.UTC, `{"action":"add","message":"m","at":"2026-10-21 10:30"}`,
			Schedule{Kind: KindAt, At: time.Date(2026, 10, 21, 10, 30, 0, 0, time.UTC)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, reg, _ := newToolHarness(t, ToolConfig{Location: tt.loc})
			if res := call(t, reg, ctx, tt.args); res.IsError {
				t.Fatalf("add = %+v", res)
			}
			jobs, _ := svc.List(true)
			if len(jobs) != 1 {
				t.Fatalf("jobs = %v", jobs)
			}
			got := jobs[0]
			if got.Schedule.Kind != tt.want.Kind || got.Schedule.Expr != tt.want.Expr || got.Schedule.TZ != tt.want.TZ ||
				!got.Schedule.At.Equal(tt.want.At) || got.DeleteAfterRun != tt.deletes {
				t.Errorf("job = %+v, want schedule %+v", got, tt.want)
			}
		})
	}
}

func TestToolAddErrors(t *testing.T) {
	chat := tools.WithTurn(context.Background(), chatTurn)
	cronTurn := tools.WithTurn(context.Background(), tools.Turn{Channel: "telegram", ChatID: "42", SessionKey: "telegram:42",
		Metadata: map[string]any{TriggerMetaKey: map[string]any{"job_id": "x"}}})
	tests := []struct {
		name string
		ctx  context.Context
		args string
		want string
	}{
		{"no turn", context.Background(), `{"action":"add","message":"m","every_seconds":60}`, "must be created from a chat session"},
		{"cron turn", cronTurn, `{"action":"add","message":"m","every_seconds":60}`, "from within a cron job"},
		{"no message", chat, `{"action":"add","every_seconds":60}`, "requires a non-empty 'message'"},
		{"no schedule", chat, `{"action":"add","message":"m"}`, "exactly one of"},
		{"two schedules", chat, `{"action":"add","message":"m","every_seconds":60,"cron_expr":"* * * * *"}`, "exactly one of"},
		{"tz without cron", chat, `{"action":"add","message":"m","every_seconds":60,"tz":"UTC"}`, "tz can only be used with cron_expr"},
		{"bad tz", chat, `{"action":"add","message":"m","cron_expr":"0 9 * * *","tz":"Mars/Base"}`, "unknown timezone"},
		{"bad expr", chat, `{"action":"add","message":"m","cron_expr":"every day"}`, "want 5 fields"},
		{"zero interval", chat, `{"action":"add","message":"m","every_seconds":0}`, "positive integer"},
		{"past at", chat, `{"action":"add","message":"m","at":"2026-10-20T11:00:00Z"}`, "is not in the future"},
		{"bad at", chat, `{"action":"add","message":"m","at":"tomorrow"}`, "invalid ISO datetime"},
		{"bad action", chat, `{"action":"pause"}`, "must be one of"},
		{"remove without id", chat, `{"action":"remove"}`, "job_id is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, reg, _ := newToolHarness(t, ToolConfig{Location: time.UTC})
			res := call(t, reg, tt.ctx, tt.args)
			if !res.IsError || !strings.Contains(res.Content, tt.want) {
				t.Errorf("result = %+v, want an error containing %q", res, tt.want)
			}
			if jobs, _ := svc.List(true); len(jobs) != 0 {
				t.Errorf("jobs added: %v", jobs)
			}
		})
	}
}

func TestToolListAndRemove(t *testing.T) {
	svc, reg, clock := newToolHarness(t, ToolConfig{Location: time.UTC, AddNote: "Note: runs under the gateway only."})
	ctx := tools.WithTurn(context.Background(), chatTurn)

	if res := call(t, reg, ctx, `{"action":"list"}`); res.Content != "No scheduled jobs." {
		t.Errorf("empty list = %q", res.Content)
	}
	res := call(t, reg, ctx, `{"action":"add","message":"stretch","every_seconds":5400}`)
	if !strings.HasSuffix(res.Content, "\nNote: runs under the gateway only.") {
		t.Errorf("add = %q, want the note", res.Content)
	}
	if err := svc.RegisterSystemJob(Job{ID: DreamJobID, Name: "dream", Schedule: Schedule{Kind: KindCron, Expr: "0 3 * * *", TZ: "Asia/Tokyo"}}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	jobs, _ := svc.List(false)
	id := jobs[0].ID

	list := call(t, reg, ctx, `{"action":"list"}`).Content
	for _, want := range []string{
		"Scheduled jobs:\n- stretch (id: " + id + ", every 90m)\n  Next run: 2026-10-20 13:30:00 UTC",
		"- dream (id: dream, cron: 0 3 * * * (Asia/Tokyo))\n  Purpose: Dream memory consolidation for long-term memory.\n  Protected: visible for inspection, but cannot be removed.\n  Next run: 2026-10-21 03:00:00 JST",
	} {
		if !strings.Contains(list, want) {
			t.Errorf("list =\n%s\nwant it to contain\n%s", list, want)
		}
	}

	if res := call(t, reg, ctx, `{"action":"remove","job_id":"dream"}`); !strings.HasPrefix(res.Content, "Cannot remove job `dream`.") {
		t.Errorf("remove dream = %q", res.Content)
	}
	if res := call(t, reg, ctx, `{"action":"remove","job_id":"`+id+`"}`); res.Content != "Removed job "+id {
		t.Errorf("remove = %q", res.Content)
	}
	if res := call(t, reg, ctx, `{"action":"remove","job_id":"`+id+`"}`); res.Content != "Job "+id+" not found" {
		t.Errorf("remove again = %q", res.Content)
	}
}

func TestParseAt(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	for _, tt := range []struct {
		in   string
		want time.Time
	}{
		{"2026-02-12T10:30:00", time.Date(2026, 2, 12, 10, 30, 0, 0, ny)},
		{"2026-02-12T10:30", time.Date(2026, 2, 12, 10, 30, 0, 0, ny)},
		{"2026-02-12 10:30:15.5", time.Date(2026, 2, 12, 10, 30, 15, 5e8, ny)},
		{"2026-02-12", time.Date(2026, 2, 12, 0, 0, 0, 0, ny)},
		{"2026-02-12T10:30:00Z", time.Date(2026, 2, 12, 10, 30, 0, 0, time.UTC)},
		{"2026-02-12T10:30+01:00", time.Date(2026, 2, 12, 9, 30, 0, 0, time.UTC)},
	} {
		got, err := ParseAt(tt.in, ny)
		if err != nil || !got.Equal(tt.want) {
			t.Errorf("ParseAt(%q) = %s, %v; want %s", tt.in, got, err, tt.want)
		}
	}
	if _, err := ParseAt("12/02/2026", ny); err == nil {
		t.Error("ParseAt accepted a non-ISO date")
	}
}
