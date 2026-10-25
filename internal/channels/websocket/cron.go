package websocket

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/yedamao/nanobot-go/internal/cron"
)

const (
	// CronPath lists scheduled jobs (GET) and hosts run/remove under
	// /ui/cron/{id}/run and /ui/cron/{id}.
	CronPath = "/ui/cron"
	// maxCronMessageRunes caps payload.message in list/run responses.
	maxCronMessageRunes = 500
)

// CronList is the body of GET /ui/cron.
type CronList struct {
	Jobs []CronJobView `json:"jobs"`
}

// CronJobView is one job as shown in the WebUI: schedule and run state,
// never binding metadata or absolute paths.
type CronJobView struct {
	ID             string           `json:"id"`
	Name           string           `json:"name"`
	Enabled        bool             `json:"enabled"`
	System         bool             `json:"system"`
	Schedule       CronScheduleView `json:"schedule"`
	Payload        CronPayloadView  `json:"payload"`
	Binding        *CronBindingView `json:"binding,omitempty"`
	State          CronStateView    `json:"state"`
	CreatedAt      string           `json:"createdAt,omitempty"`
	UpdatedAt      string           `json:"updatedAt,omitempty"`
	DeleteAfterRun bool             `json:"deleteAfterRun,omitempty"`
}

// CronScheduleView is the safe schedule fields plus a human summary.
type CronScheduleView struct {
	Kind    string `json:"kind"`
	At      string `json:"at,omitempty"`
	EveryMs int64  `json:"everyMs,omitempty"`
	Expr    string `json:"expr,omitempty"`
	TZ      string `json:"tz,omitempty"`
	Summary string `json:"summary"`
}

// CronPayloadView is the job's action without secrets.
type CronPayloadView struct {
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
}

// CronBindingView names the chat a job belongs to (no session key or
// metadata).
type CronBindingView struct {
	Channel string `json:"channel"`
	ChatID  string `json:"chatId"`
}

// CronStateView is run bookkeeping with RFC3339 times.
type CronStateView struct {
	NextRun    string `json:"nextRun,omitempty"`
	LastRun    string `json:"lastRun,omitempty"`
	LastStatus string `json:"lastStatus,omitempty"`
	LastError  string `json:"lastError,omitempty"`
	RunCount   int    `json:"runCount,omitempty"`
}

// cronJobView maps a cron.Job into the WebUI shape.
func cronJobView(j cron.Job) CronJobView {
	v := CronJobView{
		ID:      j.ID,
		Name:    j.Name,
		Enabled: j.Enabled,
		System:  j.System(),
		Schedule: CronScheduleView{
			Kind:    string(j.Schedule.Kind),
			Expr:    j.Schedule.Expr,
			TZ:      j.Schedule.TZ,
			Summary: j.Schedule.String(),
		},
		Payload: CronPayloadView{
			Kind:    string(j.Payload.Kind),
			Message: truncateRunes(j.Payload.Message, maxCronMessageRunes),
		},
		State: CronStateView{
			LastStatus: string(j.State.LastStatus),
			LastError:  j.State.LastError,
			RunCount:   j.State.RunCount,
		},
		DeleteAfterRun: j.DeleteAfterRun,
	}
	if !j.Schedule.At.IsZero() {
		v.Schedule.At = j.Schedule.At.UTC().Format(timeRFC3339)
	}
	if j.Schedule.Every > 0 {
		v.Schedule.EveryMs = j.Schedule.Every.Milliseconds()
	}
	if j.Binding != nil && (j.Binding.Channel != "" || j.Binding.ChatID != "") {
		v.Binding = &CronBindingView{Channel: j.Binding.Channel, ChatID: j.Binding.ChatID}
	}
	if !j.State.NextRun.IsZero() {
		v.State.NextRun = j.State.NextRun.UTC().Format(timeRFC3339)
	}
	if !j.State.LastRun.IsZero() {
		v.State.LastRun = j.State.LastRun.UTC().Format(timeRFC3339)
	}
	if !j.CreatedAt.IsZero() {
		v.CreatedAt = j.CreatedAt.UTC().Format(timeRFC3339)
	}
	if !j.UpdatedAt.IsZero() {
		v.UpdatedAt = j.UpdatedAt.UTC().Format(timeRFC3339)
	}
	return v
}

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"

func truncateRunes(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	var b strings.Builder
	i := 0
	for _, r := range s {
		if i >= n {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		i++
	}
	return b.String()
}

// serveCron answers GET /ui/cron with every job (including disabled and
// system), soonest first.
func (c *Channel) serveCron(w http.ResponseWriter, _ *http.Request) {
	jobs, err := c.cron.List(true)
	if err != nil {
		c.log.Warn("websocket: list cron jobs", "error", err)
		http.Error(w, "cannot list cron jobs", http.StatusInternalServerError)
		return
	}
	out := CronList{Jobs: make([]CronJobView, 0, len(jobs))}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, cronJobView(j))
	}
	writeJSON(w, out)
}

// runCronBody is the optional body of POST /ui/cron/{id}/run.
type runCronBody struct {
	Force bool `json:"force"`
}

// runCron answers POST /ui/cron/{id}/run: run the job now (force for a
// disabled job) and answer with the updated view.
func (c *Channel) runCron(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "job id is required", http.StatusBadRequest)
		return
	}
	var body runCronBody
	if r.Body != nil {
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<10))
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, `body must be {"force":true} or empty`, http.StatusBadRequest)
			return
		}
	}
	job, ran, err := c.cron.RunJob(r.Context(), id, body.Force)
	if !ran && err == nil {
		// Missing, or disabled without force.
		if _, ok, getErr := c.cron.Get(id); getErr != nil {
			c.log.Warn("websocket: get cron job", "error", getErr)
			http.Error(w, "cannot load the cron job", http.StatusInternalServerError)
			return
		} else if !ok {
			http.Error(w, "no such cron job", http.StatusNotFound)
			return
		}
		http.Error(w, "job is disabled; pass {\"force\":true} to run it", http.StatusConflict)
		return
	}
	if err != nil && !ran {
		c.log.Warn("websocket: run cron job", "job", id, "error", err)
		http.Error(w, "cannot run the cron job", http.StatusInternalServerError)
		return
	}
	// Ran (possibly with a recorded run error): return the updated view.
	if updated, ok, getErr := c.cron.Get(id); getErr == nil && ok {
		job = updated
	}
	writeJSON(w, cronJobView(job))
}

// removeCron answers DELETE /ui/cron/{id}. System jobs are refused.
func (c *Channel) removeCron(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "job id is required", http.StatusBadRequest)
		return
	}
	res, err := c.cron.Remove(id)
	if err != nil {
		c.log.Warn("websocket: remove cron job", "job", id, "error", err)
		http.Error(w, "cannot remove the cron job", http.StatusInternalServerError)
		return
	}
	switch res {
	case cron.Removed:
		w.WriteHeader(http.StatusNoContent)
	case cron.NotFound:
		http.Error(w, "no such cron job", http.StatusNotFound)
	case cron.Protected:
		http.Error(w, "system cron jobs cannot be removed", http.StatusForbidden)
	default:
		http.Error(w, "cannot remove the cron job", http.StatusInternalServerError)
	}
}
