package cron

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a Clock that moves only when the test advances it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
	// sleeps receives the duration of every After call, so a test can
	// wait until the service is asleep.
	sleeps chan time.Duration
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, sleeps: make(chan time.Duration, 100)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	if d <= 0 {
		ch <- c.now
	} else {
		c.waiters = append(c.waiters, fakeWaiter{at: c.now.Add(d), ch: ch})
	}
	c.mu.Unlock()
	c.sleeps <- d
	return ch
}

// Advance moves the clock and fires the timers that came due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
}

// waitSleep returns the duration of the service's next sleep.
func (c *fakeClock) waitSleep(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.sleeps:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("service did not go to sleep")
		return 0
	}
}

// recorder is a RunFunc that records the jobs it runs.
type recorder struct {
	ran   chan Job
	err   func(Job) error
	reply func(Job) string
}

func newRecorder() *recorder { return &recorder{ran: make(chan Job, 100)} }

func (r *recorder) run(ctx context.Context, j Job) (string, error) {
	r.ran <- j
	var reply string
	if r.reply != nil {
		reply = r.reply(j)
	}
	if r.err != nil {
		return reply, r.err(j)
	}
	return reply, nil
}

func (r *recorder) wait(t *testing.T) Job {
	t.Helper()
	select {
	case j := <-r.ran:
		return j
	case <-time.After(5 * time.Second):
		t.Fatal("no job ran")
		return Job{}
	}
}

func (r *recorder) none(t *testing.T) {
	t.Helper()
	select {
	case j := <-r.ran:
		t.Fatalf("unexpected run of %s (%s)", j.ID, j.Name)
	default:
	}
}

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

type harness struct {
	svc   *Service
	clock *fakeClock
	rec   *recorder
	stop  context.CancelFunc
	done  chan error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{clock: newFakeClock(t0), rec: newRecorder()}
	svc, err := NewService(ServiceConfig{Store: NewStore(t.TempDir()), Run: h.rec.run, Clock: h.clock, MaxSleep: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	return h
}

// serve starts Serve and waits for its first sleep, which it returns.
func (h *harness) serve(t *testing.T) time.Duration {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.stop = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- h.svc.Serve(ctx, context.Background()) }()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	return h.clock.waitSleep(t)
}

func (h *harness) add(t *testing.T, spec AddSpec) Job {
	t.Helper()
	if spec.Message == "" {
		spec.Message = "do the thing"
	}
	if spec.Binding == nil {
		spec.Binding = &Binding{Channel: "chat", ChatID: "1", SessionKey: "chat:1"}
	}
	j, err := h.svc.Add(spec)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (h *harness) get(t *testing.T, id string) (Job, bool) {
	t.Helper()
	j, ok, err := h.svc.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return j, ok
}

func TestServiceRunsDueJobOnceAndRearms(t *testing.T) {
	h := newHarness(t)
	job := h.add(t, AddSpec{Name: "tick", Schedule: Schedule{Kind: KindEvery, Every: 2 * time.Minute}})
	if !job.State.NextRun.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("NextRun = %s", job.State.NextRun)
	}
	if d := h.serve(t); d != 2*time.Minute {
		t.Fatalf("first sleep = %s, want 2m", d)
	}

	h.clock.Advance(2 * time.Minute)
	if got := h.rec.wait(t); got.ID != job.ID || got.Payload.Message != "do the thing" {
		t.Errorf("ran %+v", got)
	}
	// Re-armed for the next interval, counted from the run.
	if d := h.clock.waitSleep(t); d != 2*time.Minute {
		t.Errorf("sleep after the run = %s, want 2m", d)
	}
	h.rec.none(t)
	j, _ := h.get(t, job.ID)
	if j.State.RunCount != 1 || j.State.LastStatus != StatusOK || !j.State.LastRun.Equal(t0.Add(2*time.Minute)) ||
		!j.State.NextRun.Equal(t0.Add(4*time.Minute)) || !j.Enabled {
		t.Errorf("state after run = %+v", j.State)
	}

	h.clock.Advance(2 * time.Minute)
	h.rec.wait(t)
	h.clock.waitSleep(t)
	if j, _ := h.get(t, job.ID); j.State.RunCount != 2 {
		t.Errorf("RunCount = %d", j.State.RunCount)
	}
}

func TestServiceCapsSleepAndWakesOnChanges(t *testing.T) {
	h := newHarness(t)
	// Nothing scheduled: sleep the maximum.
	if d := h.serve(t); d != 5*time.Minute {
		t.Fatalf("idle sleep = %s", d)
	}
	// A far-away job still sleeps at most the maximum.
	h.add(t, AddSpec{Schedule: Schedule{Kind: KindEvery, Every: time.Hour}})
	if d := h.clock.waitSleep(t); d != 5*time.Minute {
		t.Fatalf("sleep with a job in 1h = %s", d)
	}
	// Adding a sooner job wakes the timer and re-arms it.
	soon := h.add(t, AddSpec{Schedule: Schedule{Kind: KindEvery, Every: 90 * time.Second}})
	if d := h.clock.waitSleep(t); d != 90*time.Second {
		t.Fatalf("sleep after adding a 90s job = %s", d)
	}
	// Removing it re-arms again.
	if res, err := h.svc.Remove(soon.ID); err != nil || res != Removed {
		t.Fatalf("Remove = %v, %v", res, err)
	}
	if d := h.clock.waitSleep(t); d != 5*time.Minute {
		t.Fatalf("sleep after removal = %s", d)
	}
	h.clock.Advance(5 * time.Minute)
	h.clock.waitSleep(t)
	h.rec.none(t)
}

func TestServiceAtJobs(t *testing.T) {
	h := newHarness(t)
	del := h.add(t, AddSpec{Name: "once", Schedule: Schedule{Kind: KindAt, At: t0.Add(time.Minute)}, DeleteAfterRun: true})
	keep := h.add(t, AddSpec{Name: "keep", Schedule: Schedule{Kind: KindAt, At: t0.Add(time.Minute)}})
	h.serve(t)
	h.clock.Advance(time.Minute)
	ran := map[string]bool{h.rec.wait(t).ID: true, h.rec.wait(t).ID: true}
	if !ran[del.ID] || !ran[keep.ID] {
		t.Fatalf("ran %v", ran)
	}
	if d := h.clock.waitSleep(t); d != 5*time.Minute {
		t.Errorf("sleep after the at jobs = %s", d)
	}
	if _, ok := h.get(t, del.ID); ok {
		t.Error("delete-after-run job still stored")
	}
	j, ok := h.get(t, keep.ID)
	if !ok || j.Enabled || !j.State.NextRun.IsZero() || j.State.RunCount != 1 {
		t.Errorf("kept at job = %+v (found %v)", j, ok)
	}
	h.clock.Advance(time.Hour)
	h.clock.waitSleep(t)
	h.rec.none(t)
}

func TestServiceSkipsDisabledJobs(t *testing.T) {
	h := newHarness(t)
	off := h.add(t, AddSpec{Schedule: Schedule{Kind: KindEvery, Every: time.Minute}})
	err := h.svc.Store().Update(func(jobs []Job) ([]Job, error) {
		jobs[find(jobs, off.ID)].Enabled = false
		return jobs, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := h.serve(t); d != 5*time.Minute {
		t.Errorf("sleep with only a disabled job = %s", d)
	}
	h.clock.Advance(5 * time.Minute)
	h.clock.waitSleep(t)
	h.rec.none(t)
	if jobs, _ := h.svc.List(false); len(jobs) != 0 {
		t.Errorf("List(false) = %v", jobs)
	}
	if jobs, _ := h.svc.List(true); len(jobs) != 1 {
		t.Errorf("List(true) = %v", jobs)
	}

	// RunJob refuses it without force.
	if _, ran, err := h.svc.RunJob(context.Background(), off.ID, false); ran || err != nil {
		t.Errorf("RunJob without force = %v, %v", ran, err)
	}
	if _, ran, err := h.svc.RunJob(context.Background(), off.ID, true); !ran || err != nil {
		t.Errorf("RunJob with force = %v, %v", ran, err)
	}
	h.rec.wait(t)
	if _, ran, err := h.svc.RunJob(context.Background(), "missing", true); ran || err != nil {
		t.Errorf("RunJob of a missing job = %v, %v", ran, err)
	}
}

func TestServiceRecordsFailuresAndSkips(t *testing.T) {
	h := newHarness(t)
	failing := h.add(t, AddSpec{Name: "fails", Schedule: Schedule{Kind: KindEvery, Every: time.Minute}})
	skipped := h.add(t, AddSpec{Name: "skips", Schedule: Schedule{Kind: KindEvery, Every: time.Minute}})
	h.rec.err = func(j Job) error {
		if j.ID == failing.ID {
			return errors.New("provider down")
		}
		return fmt.Errorf("no chat: %w", ErrSkipped)
	}
	h.serve(t)
	h.clock.Advance(time.Minute)
	h.rec.wait(t)
	h.rec.wait(t)
	h.clock.waitSleep(t)

	f, _ := h.get(t, failing.ID)
	if f.State.LastStatus != StatusError || f.State.LastError != "provider down" || !f.State.NextRun.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("failed job state = %+v", f.State)
	}
	s, _ := h.get(t, skipped.ID)
	if s.State.LastStatus != StatusSkipped || !strings.Contains(s.State.LastError, "no chat") || s.State.RunCount != 1 {
		t.Errorf("skipped job state = %+v", s.State)
	}
	if h := f.State.RunHistory; len(h) != 1 || h[0].Status != StatusError || h[0].Error != "provider down" || !h[0].RunAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("failed job history = %+v", h)
	}
	if h := s.State.RunHistory; len(h) != 1 || h[0].Status != StatusSkipped || !strings.Contains(h[0].Error, "no chat") {
		t.Errorf("skipped job history = %+v", h)
	}
}

// RunHistory keeps the last MaxRunHistory runs, oldest first, with each
// run's start and duration.
func TestServiceRunHistoryCapped(t *testing.T) {
	h := newHarness(t)
	job := h.add(t, AddSpec{Schedule: Schedule{Kind: KindEvery, Every: time.Hour}})
	h.rec.err = func(Job) error {
		h.clock.Advance(1500 * time.Millisecond)
		return nil
	}
	h.rec.reply = func(Job) string { return "done ✓" }
	var starts []time.Time
	for i := range MaxRunHistory + 1 {
		h.clock.Advance(time.Minute)
		starts = append(starts, h.clock.Now())
		if i == MaxRunHistory {
			h.rec.err = func(Job) error { return errors.New(strings.Repeat("x", 2*maxErrorChars)) }
			h.rec.reply = func(Job) string { return strings.Repeat("é", 2*maxReplyChars) }
		}
		if _, ran, _ := h.svc.RunJob(context.Background(), job.ID, false); !ran {
			t.Fatalf("run %d did not run", i)
		}
		h.rec.wait(t)
	}
	j, _ := h.get(t, job.ID)
	hist := j.State.RunHistory
	if len(hist) != MaxRunHistory || j.State.RunCount != MaxRunHistory+1 {
		t.Fatalf("history len = %d, runCount = %d", len(hist), j.State.RunCount)
	}
	if !hist[0].RunAt.Equal(starts[1]) || hist[0].Status != StatusOK || hist[0].DurationMs != 1500 || hist[0].Reply != "done ✓" {
		t.Errorf("oldest = %+v, want start %v", hist[0], starts[1])
	}
	last := hist[len(hist)-1]
	if !last.RunAt.Equal(starts[MaxRunHistory]) || last.Status != StatusError || len(last.Error) != maxErrorChars || last.DurationMs != 0 ||
		last.Reply != strings.Repeat("é", maxReplyChars) {
		t.Errorf("newest = %+v", last)
	}
}

// A run canceled by shutdown is not recorded: the job stays due.
func TestServiceDoesNotRecordCanceledRuns(t *testing.T) {
	h := newHarness(t)
	job := h.add(t, AddSpec{Schedule: Schedule{Kind: KindAt, At: t0.Add(time.Minute)}, DeleteAfterRun: true})
	jobCtx, cancelJobs := context.WithCancel(context.Background())
	h.rec.err = func(Job) error {
		cancelJobs()
		return context.Canceled
	}
	h.rec.reply = func(Job) string { return "partial" }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.svc.Serve(ctx, jobCtx) }()
	h.clock.waitSleep(t)
	h.clock.Advance(time.Minute)
	h.rec.wait(t)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Serve = %v", err)
	}
	j, ok := h.get(t, job.ID)
	if !ok || j.State.RunCount != 0 || len(j.State.RunHistory) != 0 || !j.State.NextRun.Equal(t0.Add(time.Minute)) {
		t.Errorf("job after a canceled run = %+v (found %v)", j, ok)
	}
}

// Serve brings the schedule up to date: overdue every/cron runs are
// skipped, an overdue at job runs once, future runs are kept.
func TestServiceRescheduleOnStart(t *testing.T) {
	h := newHarness(t)
	past, future := t0.Add(-time.Hour), t0.Add(3*time.Minute)
	jobs := []Job{
		{ID: "every", Enabled: true, Schedule: Schedule{Kind: KindEvery, Every: 10 * time.Minute}, State: State{NextRun: past}},
		{ID: "future", Enabled: true, Schedule: Schedule{Kind: KindEvery, Every: 10 * time.Minute}, State: State{NextRun: future}},
		{ID: "cron", Enabled: true, Schedule: Schedule{Kind: KindCron, Expr: "0 * * * *", TZ: "UTC"}},
		{ID: "at", Enabled: true, Schedule: Schedule{Kind: KindAt, At: past}, State: State{NextRun: past}, DeleteAfterRun: true},
	}
	for i := range jobs {
		jobs[i].Payload = Payload{Kind: PayloadAgentTurn, Message: "m"}
	}
	if err := h.svc.Store().Update(func([]Job) ([]Job, error) { return jobs, nil }); err != nil {
		t.Fatal(err)
	}
	if d := h.serve(t); d != 0 {
		t.Fatalf("first sleep = %s, want 0 for the overdue at job", d)
	}
	if got := h.rec.wait(t); got.ID != "at" {
		t.Errorf("first run = %s", got.ID)
	}
	if d := h.clock.waitSleep(t); d != 3*time.Minute {
		t.Errorf("next sleep = %s, want 3m", d)
	}
	h.rec.none(t)
	want := map[string]time.Time{
		"every":  t0.Add(10 * time.Minute),
		"future": future,
		"cron":   t0.Add(time.Hour),
	}
	for id, next := range want {
		if j, _ := h.get(t, id); !j.State.NextRun.Equal(next) {
			t.Errorf("%s NextRun = %s, want %s", id, j.State.NextRun, next)
		}
	}
}

func TestServiceAddValidates(t *testing.T) {
	h := newHarness(t)
	for _, tt := range []struct {
		spec AddSpec
		want string
	}{
		{AddSpec{Message: " ", Schedule: Schedule{Kind: KindEvery, Every: time.Minute}}, "message"},
		{AddSpec{Message: "m", Schedule: Schedule{Kind: KindEvery}}, "positive interval"},
		{AddSpec{Message: "m", Schedule: Schedule{Kind: KindAt, At: t0}}, "not in the future"},
		{AddSpec{Message: "m", Schedule: Schedule{Kind: KindCron, Expr: "0 0 30 2 *"}}, "never runs"},
	} {
		if _, err := h.svc.Add(tt.spec); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Add(%+v) = %v, want %q", tt.spec, err, tt.want)
		}
	}
	j := h.add(t, AddSpec{Message: "a  long\nmessage that goes on and on and on", Schedule: Schedule{Kind: KindEvery, Every: time.Minute}})
	if j.Name != "a long message that goes on an" || len(j.ID) != 8 {
		t.Errorf("job = %q %q", j.ID, j.Name)
	}
}

func TestServiceSystemJobs(t *testing.T) {
	h := newHarness(t)
	dream := Job{ID: "dream", Name: "dream", Schedule: Schedule{Kind: KindEvery, Every: 2 * time.Hour}}
	if err := h.svc.RegisterSystemJob(dream); err != nil {
		t.Fatal(err)
	}
	j, ok := h.get(t, "dream")
	if !ok || !j.System() || !j.State.NextRun.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("system job = %+v", j)
	}
	if res, err := h.svc.Remove("dream"); err != nil || res != Protected {
		t.Errorf("Remove(dream) = %v, %v", res, err)
	}
	// Registering it again with the same schedule keeps its progress.
	h.clock.Advance(time.Hour)
	if err := h.svc.RegisterSystemJob(dream); err != nil {
		t.Fatal(err)
	}
	if j, _ := h.get(t, "dream"); !j.State.NextRun.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("re-registered NextRun = %s", j.State.NextRun)
	}
	// A new schedule resets it.
	dream.Schedule.Every = time.Hour
	if err := h.svc.RegisterSystemJob(dream); err != nil {
		t.Fatal(err)
	}
	if j, _ := h.get(t, "dream"); !j.State.NextRun.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("rescheduled NextRun = %s", j.State.NextRun)
	}
	if err := h.svc.RemoveSystemJob("dream"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.get(t, "dream"); ok {
		t.Error("system job not removed")
	}
	if res, _ := h.svc.Remove("dream"); res != NotFound {
		t.Errorf("Remove of a missing job = %v", res)
	}
}

func TestServiceWithoutRunFunc(t *testing.T) {
	svc, err := NewService(ServiceConfig{Store: NewStore(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Serve(context.Background(), context.Background()); err == nil {
		t.Error("Serve without a run function succeeded")
	}
	if _, _, err := svc.RunJob(context.Background(), "x", true); err == nil {
		t.Error("RunJob without a run function succeeded")
	}
	if _, err := NewService(ServiceConfig{}); err == nil {
		t.Error("NewService without a store succeeded")
	}
}

func TestUpdateJob(t *testing.T) {
	svc, err := NewService(ServiceConfig{Store: NewStore(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.Add(AddSpec{
		Name:     "n1",
		Message:  "ping",
		Schedule: Schedule{Kind: KindEvery, Every: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterSystemJob(Job{
		ID: "sys", Name: "sys",
		Schedule: Schedule{Kind: KindEvery, Every: 2 * time.Hour},
	}); err != nil {
		t.Fatal(err)
	}

	name := "renamed"
	msg := "pong"
	enabled := false
	got, res, err := svc.Update(job.ID, UpdateSpec{Name: &name, Message: &msg, Enabled: &enabled})
	if err != nil || res != Updated {
		t.Fatalf("Update = %v %v %v", got, res, err)
	}
	if got.Name != "renamed" || got.Payload.Message != "pong" || got.Enabled || !got.State.NextRun.IsZero() {
		t.Errorf("got = %+v", got)
	}

	_, res, err = svc.Update("sys", UpdateSpec{Name: &name})
	if err != nil || res != UpdateProtected {
		t.Fatalf("system Update = %v %v", res, err)
	}
	_, res, err = svc.Update("missing", UpdateSpec{Name: &name})
	if err != nil || res != UpdateNotFound {
		t.Fatalf("missing Update = %v %v", res, err)
	}

	every := Schedule{Kind: KindEvery, Every: 30 * time.Minute}
	on := true
	got, res, err = svc.Update(job.ID, UpdateSpec{Schedule: &every, Enabled: &on})
	if err != nil || res != Updated || got.Schedule.Every != 30*time.Minute || got.State.NextRun.IsZero() {
		t.Fatalf("reschedule = %+v %v %v", got, res, err)
	}
}
