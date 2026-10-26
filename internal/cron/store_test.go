package cron

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleJobs() []Job {
	created := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	return []Job{
		{
			ID: "a1b2c3d4", Name: "standup", Enabled: true,
			Schedule: Schedule{Kind: KindCron, Expr: "0 9 * * 1-5", TZ: "Europe/Berlin"},
			Payload:  Payload{Kind: PayloadAgentTurn, Message: "remind me of the standup"},
			Binding:  &Binding{Channel: "telegram", ChatID: "42", SessionKey: "telegram:42", Metadata: map[string]any{"thread_id": "7"}},
			State: State{NextRun: created.Add(time.Hour), LastRun: created.Add(-time.Hour),
				LastStatus: StatusError, LastError: "boom", RunCount: 3,
				RunHistory: []RunRecord{
					{RunAt: created.Add(-2 * time.Hour), Status: StatusOK, DurationMs: 1200, Reply: "Standup in 5 minutes."},
					{RunAt: created.Add(-time.Hour), Status: StatusError, DurationMs: 40, Error: "boom"},
				}},
			CreatedAt: created, UpdatedAt: created,
		},
		{
			ID: "e5f6a7b8", Name: "water", Enabled: false,
			Schedule:  Schedule{Kind: KindEvery, Every: 90 * time.Minute},
			Payload:   Payload{Kind: PayloadAgentTurn, Message: "drink water"},
			CreatedAt: created, UpdatedAt: created,
		},
		{
			ID: "dream", Name: "dream", Enabled: true,
			Schedule:  Schedule{Kind: KindAt, At: created.Add(24 * time.Hour)},
			Payload:   Payload{Kind: PayloadSystem},
			CreatedAt: created, UpdatedAt: created, DeleteAfterRun: true,
		},
	}
}

func TestStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	if got := s.Path(); got != filepath.Join(root, "cron", "jobs.json") {
		t.Errorf("Path = %s", got)
	}
	if jobs, err := s.Load(); err != nil || len(jobs) != 0 {
		t.Fatalf("Load of a missing store = %v, %v", jobs, err)
	}
	want := sampleJobs()
	if err := s.Update(func([]Job) ([]Job, error) { return want, nil }); err != nil {
		t.Fatal(err)
	}
	got, err := NewStore(root).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip:\n got  %+v\n want %+v", got, want)
	}

	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{`"version": 1`, `"everyMs": 5400000`, `"tz": "Europe/Berlin"`, `"sessionKey": "telegram:42"`, `"kind": "system_event"`, `"runHistory": [`, `"durationMs": 1200`, `"reply": "Standup in 5 minutes."`} {
		if !strings.Contains(string(data), frag) {
			t.Errorf("jobs.json lacks %s:\n%s", frag, data)
		}
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(s.Path()); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("jobs.json mode = %v, %v", fi.Mode(), err)
		}
	}
}

func TestStoreAtomicWrite(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Update(func([]Job) ([]Job, error) { return sampleJobs(), nil }); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.Path())

	// A failing update writes nothing.
	boom := errors.New("boom")
	if err := s.Update(func(jobs []Job) ([]Job, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("Update = %v", err)
	}
	if after, _ := os.ReadFile(s.Path()); string(after) != string(before) {
		t.Error("failed update changed the file")
	}
	// Only jobs.json and the lock file remain: no temp files.
	entries, err := os.ReadDir(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if fmt.Sprint(names) != "[.jobs.lock jobs.json]" {
		t.Errorf("cron dir = %v", names)
	}
}

func TestStoreRefusesCorruptFile(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	const garbage = `{"jobs": [` // truncated
	if err := os.WriteFile(s.Path(), []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("Load = %v, want a corrupt-store error", err)
	}
	called := false
	err := s.Update(func(jobs []Job) ([]Job, error) { called = true; return jobs, nil })
	if err == nil || called {
		t.Errorf("Update on a corrupt store = %v (fn called: %v)", err, called)
	}
	if data, _ := os.ReadFile(s.Path()); string(data) != garbage {
		t.Errorf("corrupt store overwritten with %q", data)
	}

	if err := os.WriteFile(s.Path(), []byte(`{"version": 99, "jobs": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil || !strings.Contains(err.Error(), "version 99") {
		t.Errorf("Load of a newer store = %v", err)
	}
}

// Updates through separate Stores (as separate processes would use) are
// serialized by the file lock, so none is lost.
func TestStoreConcurrentUpdates(t *testing.T) {
	root := t.TempDir()
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			s := NewStore(root)
			err := s.Update(func(jobs []Job) ([]Job, error) {
				return append(jobs, Job{ID: fmt.Sprint(i), Name: "j"}), nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	jobs, err := NewStore(root).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != n {
		t.Errorf("jobs = %d, want %d", len(jobs), n)
	}
}
