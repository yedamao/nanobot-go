package weixin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/pairing"
)

const (
	testToken = "bot-token-SECRET-123@im.bot"
	alice     = "alice@im.wechat"
	bob       = "bob@im.wechat"
)

// apiCall is one request the fake iLink server received.
type apiCall struct {
	Endpoint string // last path element, e.g. getupdates
	Query    map[string][]string
	Header   http.Header
	Body     map[string]any
	Host     string
}

// reply answers a request instead of the default; ok false falls
// through to the default.
type replyFunc func(c apiCall, n int) (status int, body string, ok bool)

// fakeILink is an in-process iLink bot API. getupdates long-polls: it
// answers with the next queued response, or blocks until the request
// ends.
type fakeILink struct {
	t   *testing.T
	srv *httptest.Server

	updates chan string

	mu     sync.Mutex
	calls  []apiCall
	counts map[string]int
	reply  replyFunc
	notify chan string
	// pushed and served count the queued getupdates responses; servedBy
	// is the getupdates call that took the last one.
	pushed, served, servedBy int
}

func newFakeILink(t *testing.T) *fakeILink {
	t.Helper()
	f := &fakeILink{t: t, updates: make(chan string, 16), counts: map[string]int{}, notify: make(chan string, 1024)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeILink) setReply(r replyFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = r
}

func (f *fakeILink) serve(w http.ResponseWriter, r *http.Request) {
	endpoint := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
	call := apiCall{Endpoint: endpoint, Query: r.URL.Query(), Header: r.Header.Clone(), Host: r.Host}
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		if err := json.Unmarshal(data, &call.Body); err != nil {
			f.t.Errorf("%s: bad JSON body %q", endpoint, data)
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.counts[endpoint]++
	n := f.counts[endpoint]
	reply := f.reply
	f.mu.Unlock()
	select {
	case f.notify <- endpoint:
	default:
	}
	w.Header().Set("Content-Type", "application/json")
	if reply != nil {
		if status, body, ok := reply(call, n); ok {
			w.WriteHeader(status)
			io.WriteString(w, body)
			return
		}
	}
	switch endpoint {
	case "getupdates":
		select {
		case body := <-f.updates:
			io.WriteString(w, body)
			f.mu.Lock()
			f.served++
			f.servedBy = n
			f.mu.Unlock()
		case <-r.Context().Done():
		}
	case "getconfig":
		io.WriteString(w, `{"ret":0,"typing_ticket":"ticket-1"}`)
	default:
		io.WriteString(w, `{"ret":0}`)
	}
}

// callsOf returns the requests to endpoint so far.
func (f *fakeILink) callsOf(endpoint string) []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []apiCall
	for _, c := range f.calls {
		if c.Endpoint == endpoint {
			out = append(out, c)
		}
	}
	return out
}

// waitFor waits until endpoint has been called n times.
func (f *fakeILink) waitFor(endpoint string, n int) []apiCall {
	f.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if calls := f.callsOf(endpoint); len(calls) >= n {
			return calls
		}
		select {
		case <-f.notify:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			f.t.Fatalf("timed out waiting for %d %s calls; got %d", n, endpoint, len(f.callsOf(endpoint)))
		}
	}
}

// push queues one getupdates response.
func (f *fakeILink) push(msgs []map[string]any, cursor string) {
	data, _ := json.Marshal(map[string]any{"ret": 0, "msgs": msgs, "get_updates_buf": cursor})
	f.pushRaw(string(data))
}

// pushRaw queues one getupdates response body.
func (f *fakeILink) pushRaw(body string) {
	f.mu.Lock()
	f.pushed++
	f.mu.Unlock()
	f.updates <- body
}

// handled waits until every queued response was served and the poller
// asked for more, so the responses were handled.
func (f *fakeILink) handled() {
	f.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		done := f.served == f.pushed && f.counts["getupdates"] > f.servedBy
		f.mu.Unlock()
		if done {
			return
		}
		select {
		case <-f.notify:
		case <-time.After(5 * time.Millisecond):
		case <-deadline:
			f.t.Fatal("timed out waiting for the poller to handle the queued responses")
		}
	}
}

// lockedBuffer collects log output from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness is a channel wired to a fake iLink server and a bus.
type harness struct {
	f      *fakeILink
	ch     *Channel
	bus    *bus.MessageBus
	store  *pairing.Store
	state  stateFile
	clock  *clock
	sleeps chan time.Duration
	logs   *lockedBuffer
}

// newHarness builds (but does not start) a channel logged in with
// testToken through account.json. mutate adjusts the config section.
func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	f := newFakeILink(t)
	dir := t.TempDir()
	cfg := Config{Enabled: true, AllowFrom: []string{alice}, BaseURL: f.srv.URL, PollTimeout: 1,
		StateDir: filepath.Join(dir, "state")}
	if mutate != nil {
		mutate(&cfg)
	}
	h := &harness{f: f, bus: bus.NewMessageBus(16), store: pairing.NewStore(filepath.Join(dir, "pairing.json")),
		state: stateFile{cfg.StatePath("")}, clock: &clock{t: time.Unix(1_700_000_000, 0)},
		sleeps: make(chan time.Duration, 64), logs: &lockedBuffer{}}
	h.saveLogin(t, testToken)
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ch, err := New(cfg, channels.Deps{Bus: h.bus, Logger: log}, Options{Pairing: h.store, HTTPClient: f.srv.Client(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	h.ch = ch
	t.Cleanup(func() {
		if strings.Contains(h.logs.String(), "SECRET") {
			t.Errorf("the log leaks the token:\n%s", h.logs)
		}
	})
	// Waits are recorded and cut short, but still end with ctx.
	ch.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case h.sleeps <- d:
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
			return nil
		}
	}
	ch.now = h.clock.now
	ch.typingInterval = time.Hour
	return h
}

// saveLogin writes a login with token to account.json, as the login
// command does.
func (h *harness) saveLogin(t *testing.T, token string) {
	t.Helper()
	err := h.state.update(func(st *accountState) bool {
		*st = accountState{Token: token, BaseURL: h.f.srv.URL}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (h *harness) start(t *testing.T) {
	t.Helper()
	if err := h.ch.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.ch.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
}

// inbound waits for the next inbound bus message.
func (h *harness) inbound(t *testing.T) bus.InboundMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := h.bus.ConsumeInbound(ctx)
	if err != nil {
		t.Fatalf("no inbound message: %v", err)
	}
	return msg
}

// noInbound checks that nothing reached the bus once the queued
// responses were handled.
func (h *harness) noInbound(t *testing.T) {
	t.Helper()
	h.f.handled()
	if got := h.bus.InboundLen(); got != 0 {
		msg, _ := h.bus.ConsumeInbound(context.Background())
		t.Fatalf("unexpected inbound message: %+v", msg)
	}
}

// sync waits until the queued responses were handled.
func (h *harness) sync(t *testing.T) {
	t.Helper()
	h.f.handled()
}

// text builds an inbound text message.
func text(id, from, ctxToken, body string) map[string]any {
	return map[string]any{"message_id": id, "from_user_id": from, "to_user_id": "bot@im.bot",
		"message_type": 1, "context_token": ctxToken,
		"item_list": []any{map[string]any{"type": itemText, "text_item": map[string]any{"text": body}}}}
}

// sentTexts returns the text and context_token of every sendmessage.
func sentTexts(calls []apiCall) (texts, ctxTokens, clientIDs []string) {
	for _, c := range calls {
		msg, _ := c.Body["msg"].(map[string]any)
		items, _ := msg["item_list"].([]any)
		if len(items) == 1 {
			ti, _ := items[0].(map[string]any)["text_item"].(map[string]any)
			s, _ := ti["text"].(string)
			texts = append(texts, s)
		}
		tok, _ := msg["context_token"].(string)
		id, _ := msg["client_id"].(string)
		ctxTokens = append(ctxTokens, tok)
		clientIDs = append(clientIDs, id)
	}
	return texts, ctxTokens, clientIDs
}
