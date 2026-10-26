package cron

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultMaxSleep caps how long the timer sleeps, like nanobot's
// max_sleep_ms: jobs added by other processes (`nanobot agent`) are
// noticed within this long.
const DefaultMaxSleep = 5 * time.Minute

// maxErrorChars caps State.LastError.
const maxErrorChars = 500

// maxReplyChars caps RunRecord.Reply, keeping jobs.json bounded.
const maxReplyChars = 3000

// ErrSkipped marks a run the RunFunc deliberately did not do (a job
// without a chat to run in). Wrap it to give the reason; the run is
// recorded as skipped instead of failed.
var ErrSkipped = errors.New("skipped")

// RunFunc runs a job and returns the agent's reply, if any (nanobot's
// CronRunResult.response). The Service records the outcome and the reply,
// truncated to maxReplyChars.
type RunFunc func(ctx context.Context, job Job) (reply string, err error)

// Clock is the Service's time source, replaceable in tests.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d has passed.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// ServiceConfig wires a Service.
type ServiceConfig struct {
	Store *Store
	// Run runs due jobs. Nil makes a Service that only manages jobs
	// (`nanobot agent`, `nanobot cron list`); Serve and RunJob need it.
	Run RunFunc
	// Clock defaults to the system clock.
	Clock Clock
	// MaxSleep defaults to DefaultMaxSleep.
	MaxSleep time.Duration
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Service manages jobs and, while Serve runs, runs them when due. It
// ports nanobot's CronService: one timer goroutine sleeps until the
// earliest due job (at most MaxSleep, and is woken early by changes made
// through this Service), then runs every due job in turn and records the
// outcome. All state lives in the Store, so several Services (one per
// process) can manage the same jobs; only one should Serve.
type Service struct {
	store    *Store
	run      RunFunc
	clock    Clock
	maxSleep time.Duration
	log      *slog.Logger

	// kick wakes the timer after a change.
	kick chan struct{}
	// runMu serializes runs, so a manual RunJob never overlaps a timed
	// run.
	runMu sync.Mutex
}

// NewService returns a Service.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("cron service: store is required")
	}
	s := &Service{
		store:    cfg.Store,
		run:      cfg.Run,
		clock:    cfg.Clock,
		maxSleep: cfg.MaxSleep,
		log:      cfg.Logger,
		kick:     make(chan struct{}, 1),
	}
	if s.clock == nil {
		s.clock = realClock{}
	}
	if s.maxSleep <= 0 {
		s.maxSleep = DefaultMaxSleep
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Store returns the job store.
func (s *Service) Store() *Store { return s.store }

// AddSpec describes a new job.
type AddSpec struct {
	// Name defaults to the start of Message.
	Name     string
	Schedule Schedule
	Message  string
	Binding  *Binding
	// DeleteAfterRun removes an at job after it runs.
	DeleteAfterRun bool
}

// Add validates and stores a new agent-turn job.
func (s *Service) Add(spec AddSpec) (Job, error) {
	if strings.TrimSpace(spec.Message) == "" {
		return Job{}, errors.New("message must not be empty")
	}
	if err := spec.Schedule.Validate(); err != nil {
		return Job{}, err
	}
	now := s.clock.Now()
	next, ok := spec.Schedule.Next(now)
	if !ok {
		if spec.Schedule.Kind == KindAt {
			return Job{}, fmt.Errorf("one-time job time %s is not in the future", spec.Schedule.At.Format(time.RFC3339))
		}
		return Job{}, errors.New("schedule never runs")
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		name = truncate(strings.Join(strings.Fields(spec.Message), " "), 30)
	}
	job := Job{
		Name:           name,
		Enabled:        true,
		Schedule:       spec.Schedule,
		Payload:        Payload{Kind: PayloadAgentTurn, Message: spec.Message},
		Binding:        spec.Binding,
		State:          State{NextRun: next},
		CreatedAt:      now,
		UpdatedAt:      now,
		DeleteAfterRun: spec.DeleteAfterRun,
	}
	err := s.store.Update(func(jobs []Job) ([]Job, error) {
		for {
			job.ID = newID()
			if find(jobs, job.ID) < 0 {
				return append(jobs, job), nil
			}
		}
	})
	if err != nil {
		return Job{}, err
	}
	s.notify()
	s.log.Info("cron: added job", "job", job.ID, "name", job.Name, "schedule", job.Schedule.String())
	return job, nil
}

// List returns the jobs, soonest first; jobs that will not run again
// come last. Disabled jobs are left out unless includeDisabled.
func (s *Service) List(includeDisabled bool) ([]Job, error) {
	jobs, err := s.store.Load()
	if err != nil {
		return nil, err
	}
	if !includeDisabled {
		jobs = slices.DeleteFunc(jobs, func(j Job) bool { return !j.Enabled })
	}
	slices.SortStableFunc(jobs, func(a, b Job) int {
		az, bz := a.State.NextRun.IsZero(), b.State.NextRun.IsZero()
		if az != bz {
			if az {
				return 1
			}
			return -1
		}
		return a.State.NextRun.Compare(b.State.NextRun)
	})
	return jobs, nil
}

// Get returns the job with id.
func (s *Service) Get(id string) (Job, bool, error) {
	jobs, err := s.store.Load()
	if err != nil {
		return Job{}, false, err
	}
	if i := find(jobs, id); i >= 0 {
		return jobs[i], true, nil
	}
	return Job{}, false, nil
}

// RemoveResult is the outcome of Remove.
type RemoveResult int

// Remove outcomes.
const (
	Removed RemoveResult = iota
	NotFound
	// Protected means the job is a system job, which Remove refuses.
	Protected
)

// Remove deletes the job with id unless it is a system job.
func (s *Service) Remove(id string) (RemoveResult, error) {
	res := NotFound
	err := s.store.Update(func(jobs []Job) ([]Job, error) {
		i := find(jobs, id)
		switch {
		case i < 0:
			res = NotFound
		case jobs[i].System():
			res = Protected
		default:
			res = Removed
			jobs = slices.Delete(jobs, i, i+1)
		}
		return jobs, nil
	})
	if err != nil {
		return NotFound, err
	}
	if res == Removed {
		s.notify()
		s.log.Info("cron: removed job", "job", id)
	}
	return res, nil
}

// UpdateSpec is a partial update for an agent-turn job. Nil pointer
// fields are left unchanged. Schedule, when set, replaces the whole
// schedule and recomputes NextRun when the job is enabled.
type UpdateSpec struct {
	Name           *string
	Message        *string
	Schedule       *Schedule
	Enabled        *bool
	DeleteAfterRun *bool
	// Binding, when non-nil, replaces the chat binding. A Binding with
	// empty Channel and ChatID clears it.
	Binding *Binding
}

// UpdateResult is the outcome of Update.
type UpdateResult int

// Update outcomes.
const (
	Updated UpdateResult = iota
	// UpdateNotFound means no job with that id exists.
	UpdateNotFound
	// UpdateProtected means the job is a system job, which Update refuses.
	UpdateProtected
)

// Update changes mutable fields of the job with id. System jobs are
// refused. An empty Message is rejected. When the schedule changes (or
// the job is re-enabled), NextRun is recomputed; disabling clears it.
func (s *Service) Update(id string, patch UpdateSpec) (Job, UpdateResult, error) {
	if patch.Message != nil && strings.TrimSpace(*patch.Message) == "" {
		return Job{}, Updated, errors.New("message must not be empty")
	}
	if patch.Schedule != nil {
		if err := patch.Schedule.Validate(); err != nil {
			return Job{}, Updated, err
		}
	}
	res := UpdateNotFound
	var out Job
	err := s.store.Update(func(jobs []Job) ([]Job, error) {
		i := find(jobs, id)
		if i < 0 {
			res = UpdateNotFound
			return jobs, nil
		}
		if jobs[i].System() {
			res = UpdateProtected
			return jobs, nil
		}
		j := jobs[i]
		now := s.clock.Now()
		scheduleChanged := false
		if patch.Schedule != nil {
			if *patch.Schedule != j.Schedule {
				scheduleChanged = true
			}
			j.Schedule = *patch.Schedule
		}
		if patch.Name != nil {
			name := strings.TrimSpace(*patch.Name)
			if name == "" {
				msg := j.Payload.Message
				if patch.Message != nil {
					msg = *patch.Message
				}
				name = truncate(strings.Join(strings.Fields(msg), " "), 30)
			}
			j.Name = name
		}
		if patch.Message != nil {
			j.Payload.Message = *patch.Message
			j.Payload.Kind = PayloadAgentTurn
		}
		if patch.DeleteAfterRun != nil {
			j.DeleteAfterRun = *patch.DeleteAfterRun
		}
		if patch.Binding != nil {
			if patch.Binding.Channel == "" && patch.Binding.ChatID == "" {
				j.Binding = nil
			} else {
				b := *patch.Binding
				j.Binding = &b
			}
		}
		enabledChanged := false
		if patch.Enabled != nil && *patch.Enabled != j.Enabled {
			j.Enabled = *patch.Enabled
			enabledChanged = true
		}
		switch {
		case !j.Enabled:
			j.State.NextRun = time.Time{}
		case scheduleChanged || (enabledChanged && j.Enabled):
			next, ok := j.Schedule.Next(now)
			if !ok {
				if j.Schedule.Kind == KindAt {
					return jobs, fmt.Errorf("one-time job time %s is not in the future", j.Schedule.At.Format(time.RFC3339))
				}
				return jobs, errors.New("schedule never runs")
			}
			j.State.NextRun = next
		}
		j.UpdatedAt = now
		jobs[i] = j
		out = j
		res = Updated
		return jobs, nil
	})
	if err != nil {
		return Job{}, Updated, err
	}
	if res == Updated {
		s.notify()
		s.log.Info("cron: updated job", "job", out.ID, "name", out.Name)
	}
	return out, res, nil
}

// RegisterSystemJob adds or replaces the system job with job.ID,
// resetting its state (idempotent on restart, like nanobot's
// register_system_job).
func (s *Service) RegisterSystemJob(job Job) error {
	if job.ID == "" {
		return errors.New("system job needs an id")
	}
	if err := job.Schedule.Validate(); err != nil {
		return fmt.Errorf("system job %s: %w", job.ID, err)
	}
	now := s.clock.Now()
	job.Payload.Kind = PayloadSystem
	job.Enabled = true
	job.Binding = nil
	next, _ := job.Schedule.Next(now)
	job.State = State{NextRun: next}
	job.CreatedAt, job.UpdatedAt = now, now
	err := s.store.Update(func(jobs []Job) ([]Job, error) {
		if i := find(jobs, job.ID); i >= 0 {
			if old := jobs[i]; old.System() && old.Schedule == job.Schedule {
				job.State, job.CreatedAt = old.State, old.CreatedAt // unchanged: keep its progress
			}
			jobs[i] = job
			return jobs, nil
		}
		return append(jobs, job), nil
	})
	if err != nil {
		return err
	}
	s.notify()
	return nil
}

// RemoveSystemJob deletes the system job with id, if any.
func (s *Service) RemoveSystemJob(id string) error {
	removed := false
	err := s.store.Update(func(jobs []Job) ([]Job, error) {
		if i := find(jobs, id); i >= 0 && jobs[i].System() {
			removed = true
			return slices.Delete(jobs, i, i+1), nil
		}
		return jobs, nil
	})
	if removed {
		s.notify()
	}
	return err
}

// notify wakes the timer, if it is waiting.
func (s *Service) notify() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Serve runs due jobs until ctx ends, then returns ctx.Err(). Each run
// gets jobCtx instead, so a caller can stop starting jobs (cancel ctx)
// while letting the run in progress finish; Serve returns once that run
// is recorded. It first brings the stored schedule up to date: runs
// missed while nothing was serving are skipped for every and cron jobs,
// as in nanobot, but an at job that came due meanwhile runs once right
// away.
func (s *Service) Serve(ctx, jobCtx context.Context) error {
	if s.run == nil {
		return errors.New("cron service: no run function")
	}
	if err := s.reschedule(); err != nil {
		return err
	}
	// Changes made before now are already in the store.
	select {
	case <-s.kick:
	default:
	}
	for {
		var wait time.Duration
		if next, ok, err := s.nextWake(); err != nil {
			s.log.Error("cron: read jobs", "error", err)
			wait = s.maxSleep
		} else if ok {
			wait = min(s.maxSleep, max(0, next.Sub(s.clock.Now())))
		} else {
			wait = s.maxSleep
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.kick:
			continue
		case <-s.clock.After(wait):
		}
		s.runDue(ctx, jobCtx)
	}
}

// reschedule fills in the next run of every enabled job that has none
// or whose next run passed while nothing was serving.
func (s *Service) reschedule() error {
	now := s.clock.Now()
	count := 0
	err := s.store.Update(func(jobs []Job) ([]Job, error) {
		count = len(jobs)
		for i := range jobs {
			j := &jobs[i]
			if !j.Enabled {
				continue
			}
			if j.Schedule.Kind == KindAt && !j.State.NextRun.IsZero() {
				continue // due or not, it runs once
			}
			if j.State.NextRun.IsZero() || j.State.NextRun.Before(now) {
				j.State.NextRun, _ = j.Schedule.Next(now)
			}
		}
		return jobs, nil
	})
	if err != nil {
		return fmt.Errorf("cron service: %w", err)
	}
	s.log.Info("cron service started", "jobs", count)
	return nil
}

// nextWake returns the earliest next run of an enabled job.
func (s *Service) nextWake() (time.Time, bool, error) {
	jobs, err := s.store.Load()
	if err != nil {
		return time.Time{}, false, err
	}
	var next time.Time
	for _, j := range jobs {
		if j.Enabled && !j.State.NextRun.IsZero() && (next.IsZero() || j.State.NextRun.Before(next)) {
			next = j.State.NextRun
		}
	}
	return next, !next.IsZero(), nil
}

// runDue runs the jobs that are due, earliest first, until ctx ends.
func (s *Service) runDue(ctx, jobCtx context.Context) {
	jobs, err := s.store.Load()
	if err != nil {
		s.log.Error("cron: read jobs", "error", err)
		return
	}
	now := s.clock.Now()
	jobs = slices.DeleteFunc(jobs, func(j Job) bool { return !j.due(now) })
	slices.SortStableFunc(jobs, func(a, b Job) int { return a.State.NextRun.Compare(b.State.NextRun) })
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		// An earlier run may have removed, disabled or rescheduled it.
		// Failures are logged where they happen.
		stillDue := func(j Job) bool { return j.due(s.clock.Now()) }
		_, _ = s.runIf(jobCtx, j.ID, stillDue)
	}
}

// errNotRun means runIf found nothing to run.
var errNotRun = errors.New("not run")

// RunJob runs the job with id now, whether or not it is due, and
// records the outcome like a timed run. A disabled job only runs with
// force. It returns the job as recorded and whether it ran; the error
// is the run's error, or why the outcome could not be recorded.
func (s *Service) RunJob(ctx context.Context, id string, force bool) (Job, bool, error) {
	if s.run == nil {
		return Job{}, false, errors.New("cron service: no run function")
	}
	job, err := s.runIf(ctx, id, func(j Job) bool { return j.Enabled || force })
	if errors.Is(err, errNotRun) {
		return job, false, nil
	}
	return job, true, err
}

// runIf runs the job with id if it exists and ok accepts it.
func (s *Service) runIf(ctx context.Context, id string, ok func(Job) bool) (Job, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	jobs, err := s.store.Load()
	if err != nil {
		s.log.Error("cron: read jobs", "error", err)
		return Job{}, err
	}
	i := find(jobs, id)
	if i < 0 {
		return Job{}, errNotRun
	}
	if !ok(jobs[i]) {
		return jobs[i], errNotRun
	}
	return s.execute(ctx, jobs[i])
}

// execute runs job and records the outcome. A run cut short because ctx
// ended (shutdown) is not recorded, so the job stays due.
func (s *Service) execute(ctx context.Context, job Job) (Job, error) {
	start := s.clock.Now()
	s.log.Info("cron: running job", "job", job.ID, "name", job.Name)
	reply, runErr := s.run(ctx, job)
	if runErr != nil && ctx.Err() != nil {
		s.log.Warn("cron: job canceled; it stays due", "job", job.ID, "error", runErr)
		return job, runErr
	}
	status, msg := StatusOK, ""
	switch {
	case errors.Is(runErr, ErrSkipped):
		status, msg = StatusSkipped, runErr.Error()
		s.log.Warn("cron: job skipped", "job", job.ID, "name", job.Name, "reason", msg)
	case runErr != nil:
		status, msg = StatusError, runErr.Error()
		s.log.Error("cron: job failed", "job", job.ID, "name", job.Name, "error", runErr)
	default:
		s.log.Info("cron: job done", "job", job.ID, "name", job.Name)
	}

	recorded := job
	err := s.store.Update(func(jobs []Job) ([]Job, error) {
		i := find(jobs, job.ID)
		if i < 0 {
			return jobs, nil // removed while it ran
		}
		j := &jobs[i]
		now := s.clock.Now()
		j.State.LastRun = start
		j.State.LastStatus = status
		j.State.LastError = truncate(msg, maxErrorChars)
		j.State.RunCount++
		j.State.RunHistory = append(j.State.RunHistory, RunRecord{
			RunAt:      start,
			Status:     status,
			DurationMs: max(0, now.Sub(start).Milliseconds()),
			Error:      j.State.LastError,
			Reply:      truncate(reply, maxReplyChars),
		})
		if n := len(j.State.RunHistory); n > MaxRunHistory {
			j.State.RunHistory = slices.Clone(j.State.RunHistory[n-MaxRunHistory:])
		}
		j.UpdatedAt = now
		if j.Schedule.Kind == KindAt {
			if j.DeleteAfterRun {
				recorded = *j
				recorded.State.NextRun = time.Time{}
				return slices.Delete(jobs, i, i+1), nil
			}
			j.Enabled = false
			j.State.NextRun = time.Time{}
		} else {
			j.State.NextRun, _ = j.Schedule.Next(now)
		}
		recorded = *j
		return jobs, nil
	})
	if err != nil {
		s.log.Error("cron: record run", "job", job.ID, "error", err)
	}
	return recorded, cmp.Or(err, runErr)
}

func newID() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
