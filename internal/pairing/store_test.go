package pairing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestStore returns a store in a temp dir with a settable clock.
func newTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	s := NewStore(filepath.Join(t.TempDir(), FileName))
	s.now = func() time.Time { return now }
	return s, &now
}

var codeRE = regexp.MustCompile(`^[A-Z0-9]{4}-[A-Z0-9]{4}$`)

func TestCodeIsStablePerSender(t *testing.T) {
	s, _ := newTestStore(t)
	a, err := s.Code("telegram", "42")
	if err != nil {
		t.Fatal(err)
	}
	if !codeRE.MatchString(a) {
		t.Errorf("code %q does not look like ABCD-EFGH", a)
	}
	again, err := s.Code("telegram", "42")
	if err != nil || again != a {
		t.Errorf("second Code = %q, %v; want %q", again, err, a)
	}
	other, err := s.Code("telegram", "43")
	if err != nil || other == a {
		t.Errorf("Code for another sender = %q, %v", other, err)
	}
	if info, err := os.Stat(s.Path()); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("store file: %v, %v", info, err)
	}
}

func TestApproveFlow(t *testing.T) {
	s, _ := newTestStore(t)
	code, err := s.Code("telegram", "42")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.IsApproved("telegram", "42"); err != nil || ok {
		t.Fatalf("approved before approval: %v, %v", ok, err)
	}
	// Lower case and without the dash still matches.
	req, ok, err := s.Approve(strings.ToLower(strings.ReplaceAll(code, "-", "")))
	if err != nil || !ok {
		t.Fatalf("Approve = %v, %v", ok, err)
	}
	if req.Channel != "telegram" || req.SenderID != "42" || req.Code != code {
		t.Errorf("request = %+v", req)
	}
	if ok, err := s.IsApproved("telegram", "42"); err != nil || !ok {
		t.Errorf("IsApproved after approval = %v, %v", ok, err)
	}
	if ok, _ := s.IsApproved("discord", "42"); ok {
		t.Error("approval leaked to another channel")
	}
	if pending, _ := s.Pending(); len(pending) != 0 {
		t.Errorf("pending after approval = %v", pending)
	}
	if _, ok, _ := s.Approve(code); ok {
		t.Error("a code was approved twice")
	}
	approved, err := s.Approved()
	if err != nil || !reflect.DeepEqual(approved, map[string][]string{"telegram": {"42"}}) {
		t.Errorf("Approved = %v, %v", approved, err)
	}
	if ok, err := s.Revoke("telegram", "42"); err != nil || !ok {
		t.Errorf("Revoke = %v, %v", ok, err)
	}
	if ok, _ := s.IsApproved("telegram", "42"); ok {
		t.Error("still approved after Revoke")
	}
	if ok, _ := s.Revoke("telegram", "42"); ok {
		t.Error("Revoke of an unknown sender reported success")
	}
}

func TestDenyAndExpiry(t *testing.T) {
	s, now := newTestStore(t)
	a, _ := s.Code("telegram", "1")
	*now = now.Add(time.Minute)
	b, _ := s.Code("telegram", "2")
	pending, err := s.Pending()
	if err != nil || len(pending) != 2 || pending[0].Code != a || pending[1].Code != b {
		t.Fatalf("Pending = %+v, %v", pending, err)
	}
	if want := pending[0].CreatedAt.Add(DefaultTTL); !pending[0].ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", pending[0].ExpiresAt, want)
	}
	if ok, err := s.Deny(a); err != nil || !ok {
		t.Errorf("Deny = %v, %v", ok, err)
	}
	if ok, _ := s.Deny(a); ok {
		t.Error("Deny of a gone code reported success")
	}
	*now = now.Add(DefaultTTL + time.Second)
	if pending, _ := s.Pending(); len(pending) != 0 {
		t.Errorf("expired requests listed: %+v", pending)
	}
	if _, ok, _ := s.Approve(b); ok {
		t.Error("an expired code was approved")
	}
	// An expired code is replaced by a fresh one.
	if c, _ := s.Code("telegram", "2"); c == b {
		t.Error("expired code reused")
	}
}

func TestReadsNanobotLayout(t *testing.T) {
	s, now := newTestStore(t)
	exp := float64(now.Add(time.Minute).Unix())
	data, _ := json.Marshal(map[string]any{
		"approved": map[string]any{"telegram": []string{"7|alice"}},
		"pending": map[string]any{
			"ABCD-EFGH": map[string]any{"channel": "telegram", "sender_id": "9", "created_at": exp - 60, "expires_at": exp, "delivery_status": "sent"},
			"BAD0-0000": map[string]any{"channel": "", "sender_id": "9", "expires_at": exp},
		},
	})
	if err := os.WriteFile(s.Path(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.IsApproved("telegram", "7|alice"); !ok {
		t.Error("approved sender from a nanobot store not found")
	}
	pending, err := s.Pending()
	if err != nil || len(pending) != 1 || pending[0].Code != "ABCD-EFGH" || pending[0].SenderID != "9" {
		t.Errorf("Pending = %+v, %v", pending, err)
	}
}

func TestCorruptStoreIsNotOverwritten(t *testing.T) {
	s, _ := newTestStore(t)
	if err := os.WriteFile(s.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Code("telegram", "1"); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("Code on a corrupt store = %v", err)
	}
	if ok, err := s.IsApproved("telegram", "1"); ok || err == nil {
		t.Errorf("IsApproved on a corrupt store = %v, %v; want false and an error", ok, err)
	}
	if data, _ := os.ReadFile(s.Path()); string(data) != "{not json" {
		t.Errorf("corrupt store rewritten: %q", data)
	}
}

func TestConcurrentStores(t *testing.T) {
	// Two Stores on one file stand in for the gateway and the CLI.
	s, _ := newTestStore(t)
	other := NewStore(s.Path())
	other.now = s.now
	var wg sync.WaitGroup
	for i := range 20 {
		st := s
		if i%2 == 1 {
			st = other
		}
		wg.Go(func() {
			if _, err := st.Code("telegram", string(rune('a'+i))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if pending, err := s.Pending(); err != nil || len(pending) != 20 {
		t.Errorf("Pending = %d requests, %v; want 20", len(pending), err)
	}
}

func TestNormalizeCodeAndReply(t *testing.T) {
	for in, want := range map[string]string{
		"abcd-efgh":   "ABCD-EFGH",
		" abcdefgh  ": "ABCD-EFGH",
		"ABC":         "ABC",
	} {
		if got := NormalizeCode(in); got != want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
	if r := Reply("ABCD-EFGH"); !strings.Contains(r, "nanobot pairing approve ABCD-EFGH") {
		t.Errorf("Reply = %q", r)
	}
	if got := PathFor("/x/y/config.json"); got != filepath.Join("/x/y", FileName) {
		t.Errorf("PathFor = %s", got)
	}
}
