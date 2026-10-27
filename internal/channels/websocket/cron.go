package websocket

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yedamao/nanobot-go/internal/cron"
	"github.com/yedamao/nanobot-go/internal/session"
)

const (
	// CronPath lists scheduled jobs (GET), creates (POST), and hosts
	// run/edit/remove under /ui/cron/{id}/run and /ui/cron/{id}; GET
	// /ui/cron/{id}/runs is the job's recent run history.
	CronPath = "/ui/cron"
	// CronTargetsPath lists the chats a job can be bound to.
	CronTargetsPath = CronPath + "/targets"
	// maxCronMessageRunes caps payload.message in list/run responses.
	maxCronMessageRunes = 500
	// maxCronTargets caps GET /ui/cron/targets.
	maxCronTargets = 200
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

// CronRunsView is the body of GET /ui/cron/{id}/runs.
type CronRunsView struct {
	JobID string `json:"jobId"`
	Name  string `json:"name"`
	// Runs is newest first.
	Runs []CronRunView `json:"runs"`
}

// CronRunView is one recorded run: outcome, truncated error and the
// agent's reply as stored (truncated by the cron service), never the
// session.
type CronRunView struct {
	RunAt      string `json:"runAt"`
	Status     string `json:"status"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Error      string `json:"error,omitempty"`
	Reply      string `json:"reply,omitempty"`
}

// CronTargets is the body of GET /ui/cron/targets.
type CronTargets struct {
	// Default is where a job created without a binding runs.
	Default CronBindingView `json:"default"`
	// Targets is the stored chats, most recently updated first.
	Targets []CronTargetView `json:"targets"`
}

// CronTargetView is one chat a job can be bound to.
type CronTargetView struct {
	Channel   string    `json:"channel"`
	ChatID    string    `json:"chatId"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// cronTargetKey splits a session key into a bindable channel and chat
// id, like heartbeat.PickTarget: keys with more parts (a Telegram
// topic's telegram:chat:topic:n) are skipped because the chat id alone
// does not route back to them, and so are sessions of channels that
// cannot deliver (cli, system, api, heartbeat).
func cronTargetKey(key string) (channel, chatID string, ok bool) {
	channel, chatID, found := strings.Cut(key, ":")
	if !found || channel == "" || chatID == "" || strings.Contains(chatID, ":") {
		return "", "", false
	}
	switch channel {
	case "cli", "system", "api", "heartbeat":
		return "", "", false
	}
	return channel, chatID, true
}

// serveCronTargets answers GET /ui/cron/targets with the stored chats a
// job can deliver to (channel, chat id and a title, never the session
// key or metadata) plus the default binding.
func (c *Channel) serveCronTargets(w http.ResponseWriter, _ *http.Request) {
	def := defaultWebCronBinding()
	out := CronTargets{
		Default: CronBindingView{Channel: def.Channel, ChatID: def.ChatID},
		Targets: []CronTargetView{},
	}
	if c.sessions == nil {
		writeJSON(w, out)
		return
	}
	infos, err := c.sessions.List()
	if err != nil {
		c.log.Warn("websocket: list cron targets", "error", err)
		http.Error(w, "cannot list chats", http.StatusInternalServerError)
		return
	}
	for _, info := range infos {
		if len(out.Targets) == maxCronTargets {
			break
		}
		channel, chatID, ok := cronTargetKey(info.Key)
		if !ok {
			continue
		}
		t := CronTargetView{Channel: channel, ChatID: chatID, UpdatedAt: info.UpdatedAt}
		if sess, err := c.sessions.Load(info.Key); err == nil {
			if ci, ok := chatInfo(chatID, sess); ok {
				t.Title = ci.Title
			}
			t.UpdatedAt = sess.UpdatedAt
		} else if !errors.Is(err, session.ErrNotFound) {
			c.log.Warn("websocket: load chat", "session", info.Key, "error", err)
		}
		out.Targets = append(out.Targets, t)
	}
	writeJSON(w, out)
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

// serveCronRuns answers GET /ui/cron/{id}/runs with the job's recent
// runs, newest first.
func (c *Channel) serveCronRuns(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "job id is required", http.StatusBadRequest)
		return
	}
	job, ok, err := c.cron.Get(id)
	if err != nil {
		c.log.Warn("websocket: get cron job", "error", err)
		http.Error(w, "cannot load the cron job", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "no such cron job", http.StatusNotFound)
		return
	}
	hist := job.State.RunHistory
	out := CronRunsView{JobID: job.ID, Name: job.Name, Runs: make([]CronRunView, 0, len(hist))}
	for i := len(hist) - 1; i >= 0; i-- {
		rec := hist[i]
		out.Runs = append(out.Runs, CronRunView{
			RunAt:      rec.RunAt.UTC().Format(timeRFC3339),
			Status:     string(rec.Status),
			DurationMs: rec.DurationMs,
			Error:      rec.Error,
			Reply:      rec.Reply,
		})
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

// cronWriteBody is the JSON body of POST /ui/cron and PATCH /ui/cron/{id}.
// Create requires message and schedule; patch fields are optional.
type cronWriteBody struct {
	Name           *string            `json:"name"`
	Message        *string            `json:"message"`
	Enabled        *bool              `json:"enabled"`
	DeleteAfterRun *bool              `json:"deleteAfterRun"`
	Schedule       *cronScheduleInput `json:"schedule"`
	Binding        *CronBindingView   `json:"binding"`
}

// cronScheduleInput is the schedule fields the page sends (kind + one of
// at / everyMs / expr).
type cronScheduleInput struct {
	Kind    string `json:"kind"`
	At      string `json:"at,omitempty"`
	EveryMs int64  `json:"everyMs,omitempty"`
	Expr    string `json:"expr,omitempty"`
	TZ      string `json:"tz,omitempty"`
}

func parseCronSchedule(in *cronScheduleInput) (cron.Schedule, error) {
	if in == nil {
		return cron.Schedule{}, errors.New("schedule is required")
	}
	kind := cron.Kind(strings.TrimSpace(in.Kind))
	switch kind {
	case cron.KindEvery:
		if in.EveryMs <= 0 {
			return cron.Schedule{}, errors.New("every schedule requires a positive everyMs")
		}
		return cron.Schedule{Kind: cron.KindEvery, Every: time.Duration(in.EveryMs) * time.Millisecond}, nil
	case cron.KindCron:
		if strings.TrimSpace(in.Expr) == "" {
			return cron.Schedule{}, errors.New("cron schedule requires expr")
		}
		s := cron.Schedule{Kind: cron.KindCron, Expr: strings.TrimSpace(in.Expr), TZ: strings.TrimSpace(in.TZ)}
		if err := s.Validate(); err != nil {
			return cron.Schedule{}, err
		}
		return s, nil
	case cron.KindAt:
		if strings.TrimSpace(in.At) == "" {
			return cron.Schedule{}, errors.New("at schedule requires at (RFC3339)")
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(in.At))
		if err != nil {
			at, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(in.At))
		}
		if err != nil {
			return cron.Schedule{}, fmt.Errorf("invalid at time %q (want RFC3339)", in.At)
		}
		return cron.Schedule{Kind: cron.KindAt, At: at.UTC()}, nil
	default:
		return cron.Schedule{}, fmt.Errorf("unknown schedule kind %q", in.Kind)
	}
}

// defaultWebCronBinding is used when create omits binding: jobs run in a
// shared websocket chat so they have somewhere to deliver.
func defaultWebCronBinding() *cron.Binding {
	return &cron.Binding{Channel: Name, ChatID: "cron", SessionKey: Name + ":cron"}
}

func bindingFromView(v *CronBindingView) *cron.Binding {
	if v == nil {
		return nil
	}
	ch := strings.TrimSpace(v.Channel)
	id := strings.TrimSpace(v.ChatID)
	if ch == "" && id == "" {
		return &cron.Binding{} // clear
	}
	if ch == "" || id == "" {
		return nil // invalid; caller checks
	}
	return &cron.Binding{Channel: ch, ChatID: id, SessionKey: ch + ":" + id}
}

// createCron answers POST /ui/cron: create an agent-turn job.
func (c *Channel) createCron(w http.ResponseWriter, r *http.Request) {
	var body cronWriteBody
	if err := decodeJSONBody(r, &body, 1<<16); err != nil {
		http.Error(w, `body must be JSON with message and schedule`, http.StatusBadRequest)
		return
	}
	if body.Message == nil || strings.TrimSpace(*body.Message) == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}
	sched, err := parseCronSchedule(body.Schedule)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	spec := cron.AddSpec{
		Message:  strings.TrimSpace(*body.Message),
		Schedule: sched,
	}
	if body.Name != nil {
		spec.Name = strings.TrimSpace(*body.Name)
	}
	if body.DeleteAfterRun != nil {
		spec.DeleteAfterRun = *body.DeleteAfterRun
	} else if sched.Kind == cron.KindAt {
		spec.DeleteAfterRun = true
	}
	switch {
	case body.Binding != nil:
		b := bindingFromView(body.Binding)
		if b == nil || (b.Channel == "" && b.ChatID == "") {
			http.Error(w, `binding needs both channel and chatId`, http.StatusBadRequest)
			return
		}
		if b.Channel != "" {
			spec.Binding = b
		}
	default:
		spec.Binding = defaultWebCronBinding()
	}
	job, err := c.cron.Add(spec)
	if err != nil {
		c.log.Warn("websocket: create cron job", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Optional enabled:false after create.
	if body.Enabled != nil && !*body.Enabled {
		off := false
		job, _, err = c.cron.Update(job.ID, cron.UpdateSpec{Enabled: &off})
		if err != nil {
			c.log.Warn("websocket: disable new cron job", "error", err)
			http.Error(w, "created but could not disable: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSONStatus(w, http.StatusCreated, cronJobView(job))
}

// patchCron answers PATCH /ui/cron/{id}: update a non-system job.
func (c *Channel) patchCron(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "job id is required", http.StatusBadRequest)
		return
	}
	var body cronWriteBody
	if err := decodeJSONBody(r, &body, 1<<16); err != nil {
		http.Error(w, `body must be a JSON object`, http.StatusBadRequest)
		return
	}
	if body.Name == nil && body.Message == nil && body.Schedule == nil &&
		body.Enabled == nil && body.DeleteAfterRun == nil && body.Binding == nil {
		http.Error(w, "no fields to update", http.StatusBadRequest)
		return
	}
	patch := cron.UpdateSpec{
		Name:           body.Name,
		Message:        body.Message,
		Enabled:        body.Enabled,
		DeleteAfterRun: body.DeleteAfterRun,
	}
	if body.Schedule != nil {
		sched, err := parseCronSchedule(body.Schedule)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		patch.Schedule = &sched
	}
	if body.Binding != nil {
		b := bindingFromView(body.Binding)
		if b == nil {
			http.Error(w, `binding needs both channel and chatId (or both empty to clear)`, http.StatusBadRequest)
			return
		}
		patch.Binding = b
		// The same chat again keeps the stored binding, so its session key
		// and metadata (thread ids and the like) survive an edit.
		if cur, ok, err := c.cron.Get(id); err == nil && ok && cur.Binding != nil &&
			cur.Binding.Channel == b.Channel && cur.Binding.ChatID == b.ChatID {
			patch.Binding = nil
		}
	}
	if patch == (cron.UpdateSpec{}) {
		// Only an unchanged binding: nothing to write.
		job, ok, err := c.cron.Get(id)
		switch {
		case err != nil:
			c.log.Warn("websocket: get cron job", "error", err)
			http.Error(w, "cannot load the cron job", http.StatusInternalServerError)
		case !ok:
			http.Error(w, "no such cron job", http.StatusNotFound)
		case job.System():
			http.Error(w, "system cron jobs cannot be edited", http.StatusForbidden)
		default:
			writeJSON(w, cronJobView(job))
		}
		return
	}
	job, res, err := c.cron.Update(id, patch)
	if err != nil {
		c.log.Warn("websocket: update cron job", "job", id, "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch res {
	case cron.Updated:
		writeJSON(w, cronJobView(job))
	case cron.UpdateNotFound:
		http.Error(w, "no such cron job", http.StatusNotFound)
	case cron.UpdateProtected:
		http.Error(w, "system cron jobs cannot be edited", http.StatusForbidden)
	default:
		http.Error(w, "cannot update the cron job", http.StatusInternalServerError)
	}
}
