package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/pairing"
)

const (
	testToken = "123456:TEST-secret_token_value"
	botID     = 999
	botName   = "nano_bot"
)

// apiCall is one request the fake Bot API received.
type apiCall struct {
	Method string
	Body   map[string]any
	// File and FileName are the uploaded file of a multipart request,
	// whose other fields are in Body as strings; FileField is its form
	// field.
	File      []byte
	FileName  string
	FileField string
	// Files are all the uploaded files by form field (sendMediaGroup
	// sends several).
	Files map[string]uploadedFile
}

type uploadedFile struct {
	Name string
	Data []byte
}

// fakeAPI is an in-process Bot API. getUpdates long-polls: it answers
// with the next queued batch, or blocks until the request ends.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	updates chan []update

	mu    sync.Mutex
	calls []apiCall
	// reply, when set, answers a method instead of the default; ok false
	// falls through to the default.
	reply func(method string, n int) (status int, body string, ok bool)
	// counts per method
	counts map[string]int
	// notify gets every method name, never blocking.
	notify chan string
	// files are served at /file/bot<token>/photos/<file_id>, which
	// getFile names as the file_path of file_id; downloads are recorded
	// as calls of method "file:<file_path>".
	files map[string][]byte
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, updates: make(chan []update, 16), counts: map[string]int{}, notify: make(chan string, 1024),
		files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if rest, ok := strings.CutPrefix(r.URL.Path, "/file/bot"); ok {
		f.serveFile(w, rest)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/bot")
	token, method, _ := strings.Cut(rest, "/")
	if !ok || token != testToken {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		return
	}
	call := apiCall{Method: method}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			f.t.Errorf("%s: bad multipart body: %v", method, err)
		} else {
			call.Body = map[string]any{}
			for k, v := range r.MultipartForm.Value {
				call.Body[k] = v[0]
			}
			for field, fhs := range r.MultipartForm.File {
				fh := fhs[0]
				call.FileName, call.FileField = fh.Filename, field
				if rd, err := fh.Open(); err == nil {
					call.File, _ = io.ReadAll(rd)
					rd.Close()
				}
				if call.Files == nil {
					call.Files = map[string]uploadedFile{}
				}
				call.Files[field] = uploadedFile{Name: fh.Filename, Data: call.File}
			}
		}
	} else {
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &call.Body); err != nil {
				f.t.Errorf("%s: bad JSON body %q", method, data)
			}
		}
	}
	body := call.Body
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.counts[method]++
	n := f.counts[method]
	reply := f.reply
	f.mu.Unlock()
	select {
	case f.notify <- method:
	default:
	}
	if reply != nil {
		if status, resp, ok := reply(method, n); ok {
			w.WriteHeader(status)
			io.WriteString(w, resp)
			return
		}
	}
	switch method {
	case "getMe":
		writeOK(w, user{ID: botID, IsBot: true, FirstName: "Nano", Username: botName})
	case "getUpdates":
		select {
		case batch := <-f.updates:
			writeOK(w, batch)
		case <-r.Context().Done():
		}
	case "sendMessage", "sendPhoto", "sendDocument":
		writeOK(w, map[string]any{"message_id": 1000 + n})
	case "sendMediaGroup":
		writeOK(w, []map[string]any{{"message_id": 2000 + n}})
	case "getFile":
		id, _ := body["file_id"].(string)
		f.mu.Lock()
		data, ok := f.files[id]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: invalid file_id"}`)
			return
		}
		writeOK(w, file{FileID: id, FileSize: int64(len(data)), FilePath: "photos/" + id})
	default:
		writeOK(w, true)
	}
}

// serveFile answers a file download; rest is "<token>/<file_path>".
func (f *fakeAPI) serveFile(w http.ResponseWriter, rest string) {
	token, filePath, _ := strings.Cut(rest, "/")
	if token != testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, apiCall{Method: "file:" + filePath})
	reply := f.reply
	id, _ := strings.CutPrefix(filePath, "photos/")
	data, ok := f.files[id]
	f.mu.Unlock()
	if reply != nil {
		if status, resp, ok := reply("file:"+filePath, 1); ok {
			w.WriteHeader(status)
			io.WriteString(w, resp)
			return
		}
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Write(data)
}

// addFile makes data downloadable as file_id id.
func (f *fakeAPI) addFile(id string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[id] = data
}

func writeOK(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// callsOf returns the requests for method so far.
func (f *fakeAPI) callsOf(method string) []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []apiCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// waitFor waits until method has been called n times.
func (f *fakeAPI) waitFor(method string, n int) []apiCall {
	f.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if calls := f.callsOf(method); len(calls) >= n {
			return calls
		}
		select {
		case <-f.notify:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			f.t.Fatalf("timed out waiting for %d %s calls; got %d", n, method, len(f.callsOf(method)))
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

// harness is a channel wired to a fake API and a bus.
type harness struct {
	f      *fakeAPI
	ch     *Channel
	bus    *bus.MessageBus
	store  *pairing.Store
	sleeps chan time.Duration
	logs   *lockedBuffer
	// workspace is the channel's Options.Workspace.
	workspace string
}

// newHarness builds (but does not start) a channel. mutate adjusts the
// config section.
func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	f := newFakeAPI(t)
	cfg := Config{Enabled: true, Token: testToken, AllowFrom: allowList{"42"}, APIBase: f.srv.URL,
		PollTimeout: 1, GroupPolicy: GroupPolicyMention}
	if mutate != nil {
		mutate(&cfg)
	}
	b := bus.NewMessageBus(16)
	store := pairing.NewStore(t.TempDir() + "/pairing.json")
	logs := &lockedBuffer{}
	ws := t.TempDir()
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ch, err := New(cfg, channels.Deps{Bus: b, Logger: log}, Options{Pairing: store, HTTPClient: f.srv.Client(), Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{f: f, ch: ch, bus: b, store: store, sleeps: make(chan time.Duration, 64), logs: logs, workspace: ws}
	t.Cleanup(func() {
		if strings.Contains(logs.String(), "secret_token") {
			t.Errorf("the log leaks the token:\n%s", logs)
		}
	})
	// Waits are recorded and skipped, but still end with ctx.
	ch.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case h.sleeps <- d:
		default:
		}
		return ctx.Err()
	}
	ch.typingInterval = time.Hour
	return h
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

// push queues one getUpdates batch.
func (h *harness) push(us ...update) { h.f.updates <- us }

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

// noInbound checks that nothing reaches the bus once the fake has served
// the next getUpdates (so the batch before it was handled).
func (h *harness) noInbound(t *testing.T) {
	t.Helper()
	n := len(h.f.callsOf("getUpdates"))
	h.f.waitFor("getUpdates", n+1)
	if got := h.bus.InboundLen(); got != 0 {
		msg, _ := h.bus.ConsumeInbound(context.Background())
		t.Fatalf("unexpected inbound message: %+v", msg)
	}
}

// Message builders.

var alice = &user{ID: 42, FirstName: "Alice", Username: "alice"}

func dm(id int64, from *user, text string) update {
	return update{UpdateID: id, Message: &message{MessageID: id * 10, From: from,
		Chat: chat{ID: from.ID, Type: "private"}, Text: text}}
}

func groupMsg(id int64, from *user, text string) update {
	return update{UpdateID: id, Message: &message{MessageID: id * 10, From: from,
		Chat: chat{ID: -100500, Type: "supergroup"}, Text: text}}
}
