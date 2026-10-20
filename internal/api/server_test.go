package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/agent"
	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/session"
	"github.com/yedamao/nanobot-go/internal/tools"
)

const testModel = "test-model"

// fakeProvider calls the lookup tool when asked to "use the tool", reports
// a tool result back, and otherwise echoes the last user message with the
// number of user messages it saw (which grows with the session history).
// Every request uses 5 input and 2 output tokens.
type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }

func (fakeProvider) Chat(_ context.Context, req providers.ChatRequest) (*providers.ChatResponse, error) {
	usage := providers.Usage{InputTokens: 5, OutputTokens: 2}
	last := req.Messages[len(req.Messages)-1]
	if last.Role == providers.RoleTool {
		return &providers.ChatResponse{Content: "tool said " + last.Content, FinishReason: providers.FinishStop, Usage: usage}, nil
	}
	if strings.Contains(last.Content, "use the tool") {
		call := providers.ToolCall{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}
		return &providers.ChatResponse{ToolCalls: []providers.ToolCall{call}, FinishReason: providers.FinishToolCalls, Usage: usage}, nil
	}
	users := 0
	for _, m := range req.Messages {
		if m.Role == providers.RoleUser {
			users++
		}
	}
	return &providers.ChatResponse{Content: fmt.Sprintf("echo %s (%d)", last.Content, users), FinishReason: providers.FinishStop, Usage: usage}, nil
}

// lookupTool records the turns it runs in.
type lookupTool struct {
	mu    sync.Mutex
	turns []tools.Turn
}

func (*lookupTool) Name() string        { return "lookup" }
func (*lookupTool) Description() string { return "looks things up" }
func (*lookupTool) Parameters() map[string]any {
	return tools.Object(map[string]any{"q": tools.String("query")})
}
func (*lookupTool) ReadOnly() bool { return true }
func (l *lookupTool) Execute(ctx context.Context, _ json.RawMessage) tools.Result {
	turn, _ := tools.TurnFrom(ctx)
	l.mu.Lock()
	l.turns = append(l.turns, turn)
	l.mu.Unlock()
	return tools.Text("42")
}

type harness struct {
	srv      *httptest.Server
	sessions *session.Manager
	tool     *lookupTool
}

// newHarness serves a Server backed by a real agent loop and the fake
// provider. mutate adjusts the Config before New.
func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	tool := &lookupTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(tool); err != nil {
		t.Fatal(err)
	}
	sessions := session.NewManager(session.NewJSONLStore(t.TempDir()))
	loop, err := agent.NewLoop(agent.LoopConfig{
		Provider: fakeProvider{},
		Tools:    reg,
		Sessions: sessions,
		Context:  agent.NewContextBuilder(t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loop.Wait)
	cfg := Config{Host: "127.0.0.1", Model: testModel, Timeout: 5 * time.Second, Turn: loop.Process}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &harness{srv: srv, sessions: sessions, tool: tool}
}

// stubServer serves a Server whose turns run turn.
func stubServer(t *testing.T, timeout time.Duration, turn TurnFunc) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New(Config{Host: "127.0.0.1", Model: testModel, Timeout: timeout, Turn: turn})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv
}

type response struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, srv *httptest.Server, method, path, auth, body string) response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{resp.StatusCode, resp.Header, string(data)}
}

func chatBody(content, sessionID string) string {
	m := map[string]any{"messages": []any{map[string]any{"role": "user", "content": content}}}
	if sessionID != "" {
		m["session_id"] = sessionID
	}
	b, _ := json.Marshal(m)
	return string(b)
}

type completionBody struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int               `json:"index"`
		Message      map[string]string `json:"message"`
		Delta        map[string]string `json:"delta"`
		FinishReason *string           `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]int `json:"usage"`
}

func decodeCompletion(t *testing.T, r response) completionBody {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, body %s", r.status, r.body)
	}
	var c completionBody
	if err := json.Unmarshal([]byte(r.body), &c); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return c
}

func (c completionBody) content() string { return c.Choices[0].Message["content"] }

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// wantError checks an OpenAI-style error response.
func wantError(t *testing.T, r response, status int, errType, message string) {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal([]byte(r.body), &e); err != nil {
		t.Fatalf("error body %q: %v", r.body, err)
	}
	if r.status != status || e.Error.Code != status || e.Error.Type != errType || !strings.Contains(e.Error.Message, message) {
		t.Errorf("got %d %s, want %d %s containing %q", r.status, r.body, status, errType, message)
	}
	if ct := r.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestNewValidation(t *testing.T) {
	turn := func(context.Context, bus.InboundMessage) (agent.Reply, error) { return agent.Reply{}, nil }
	tests := []struct {
		name   string
		cfg    Config
		errMsg string // empty means valid
	}{
		{"loopback without key", Config{Host: "127.0.0.1"}, ""},
		{"localhost without key", Config{Host: "localhost"}, ""},
		{"ipv6 loopback without key", Config{Host: "::1"}, ""},
		{"all interfaces without key", Config{Host: "0.0.0.0"}, "api.apiKey is not set"},
		{"ipv6 any without key", Config{Host: "::"}, "api.apiKey is not set"},
		{"lan without key", Config{Host: "192.168.1.5"}, "api.apiKey is not set"},
		{"blank key", Config{Host: "0.0.0.0", APIKey: "  "}, "api.apiKey is not set"},
		{"all interfaces with key", Config{Host: "0.0.0.0", APIKey: "k"}, ""},
		{"no turn", Config{Host: "127.0.0.1", Turn: nil}, "turn function"},
		{"no model", Config{Host: "127.0.0.1", Model: " "}, "model"},
		{"no timeout", Config{Host: "127.0.0.1", Timeout: -1}, "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if cfg.Turn == nil && tt.name != "no turn" {
				cfg.Turn = turn
			}
			if cfg.Model == "" {
				cfg.Model = testModel
			}
			if cfg.Timeout == 0 {
				cfg.Timeout = time.Second
			}
			_, err := New(cfg)
			switch {
			case tt.errMsg == "" && err != nil:
				t.Errorf("New = %v, want nil", err)
			case tt.errMsg != "" && (err == nil || !strings.Contains(err.Error(), tt.errMsg)):
				t.Errorf("New = %v, want containing %q", err, tt.errMsg)
			}
		})
	}
}

func TestHealthAndModels(t *testing.T) {
	h := newHarness(t, nil)
	r := do(t, h.srv, http.MethodGet, "/health", "", "")
	if r.status != http.StatusOK || strings.TrimSpace(r.body) != `{"status":"ok"}` {
		t.Errorf("GET /health = %d %q", r.status, r.body)
	}
	if r.header.Get("X-Request-ID") == "" {
		t.Error("no X-Request-ID header")
	}

	r = do(t, h.srv, http.MethodGet, "/v1/models", "", "")
	var models struct {
		Object string `json:"object"`
		Data   []struct {
			ID, Object, OwnedBy string
			Created             int64
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.body), &models); err != nil || r.status != http.StatusOK {
		t.Fatalf("GET /v1/models = %d %q (%v)", r.status, r.body, err)
	}
	if models.Object != "list" || len(models.Data) != 1 || models.Data[0].ID != testModel || models.Data[0].Object != "model" {
		t.Errorf("models = %+v", models)
	}
	if !strings.Contains(r.body, `"owned_by":"nanobot"`) {
		t.Errorf("models body = %s", r.body)
	}
}

func TestRoutingErrors(t *testing.T) {
	h := newHarness(t, nil)
	tests := []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/v1/chat/completions", http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/models", http.StatusMethodNotAllowed},
		{http.MethodPost, "/health", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/embeddings", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			wantError(t, do(t, h.srv, tt.method, tt.path, "", ""), tt.status, errInvalidRequest, "")
		})
	}
}

func TestChatCompletionRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	c := decodeCompletion(t, do(t, h.srv, http.MethodPost, "/v1/chat/completions", "", chatBody("hi", "")))
	if c.content() != "echo hi (1)" || c.Choices[0].Message["role"] != "assistant" || *c.Choices[0].FinishReason != "stop" {
		t.Errorf("choice = %+v", c.Choices)
	}
	if c.Object != "chat.completion" || c.Model != testModel || !strings.HasPrefix(c.ID, "chatcmpl-") || len(c.ID) != len("chatcmpl-")+12 || c.Created == 0 {
		t.Errorf("completion = %+v", c)
	}
	if c.Usage["prompt_tokens"] != 5 || c.Usage["completion_tokens"] != 2 || c.Usage["total_tokens"] != 7 {
		t.Errorf("usage = %v", c.Usage)
	}

	// The turn landed in the default session, which the next request
	// continues.
	c = decodeCompletion(t, do(t, h.srv, http.MethodPost, "/v1/chat/completions", "", `{"model":"test-model","messages":[{"role":"user","content":"again"}],"stream":false}`))
	if c.content() != "echo again (2)" {
		t.Errorf("second turn = %q", c.content())
	}
	sess, err := h.sessions.GetOrCreate(DefaultSessionKey)
	if err != nil || len(sess.Messages) != 4 {
		t.Errorf("session %s = %+v, %v", DefaultSessionKey, sess, err)
	}
}

func TestChatCompletionRunsTools(t *testing.T) {
	h := newHarness(t, nil)
	c := decodeCompletion(t, do(t, h.srv, http.MethodPost, "/v1/chat/completions", "", chatBody("please use the tool", "t1")))
	if c.content() != "tool said 42" {
		t.Errorf("content = %q", c.content())
	}
	// Two model requests: the tool call and the answer.
	if c.Usage["prompt_tokens"] != 10 || c.Usage["completion_tokens"] != 4 || c.Usage["total_tokens"] != 14 {
		t.Errorf("usage = %v", c.Usage)
	}
	h.tool.mu.Lock()
	defer h.tool.mu.Unlock()
	if len(h.tool.turns) != 1 {
		t.Fatalf("tool ran %d times", len(h.tool.turns))
	}
	if turn := h.tool.turns[0]; turn.Channel != Channel || turn.ChatID != "default" || turn.SessionKey != "api:t1" {
		t.Errorf("tool turn = %+v", turn)
	}
	sess, _ := h.sessions.GetOrCreate("api:t1")
	if len(sess.Messages) != 4 { // user, assistant tool call, tool result, assistant
		t.Errorf("session api:t1 has %d messages", len(sess.Messages))
	}
}

func TestSessionSeparation(t *testing.T) {
	h := newHarness(t, nil)
	post := func(content, sid string) string {
		return decodeCompletion(t, do(t, h.srv, http.MethodPost, "/v1/chat/completions", "", chatBody(content, sid))).content()
	}
	if got := post("a1", "alice"); got != "echo a1 (1)" {
		t.Errorf("alice 1 = %q", got)
	}
	if got := post("b1", "bob"); got != "echo b1 (1)" {
		t.Errorf("bob 1 = %q, want a fresh session", got)
	}
	if got := post("a2", "alice"); got != "echo a2 (2)" {
		t.Errorf("alice 2 = %q, want alice's history", got)
	}
	if got := post("d1", ""); got != "echo d1 (1)" {
		t.Errorf("default = %q, want a fresh session", got)
	}
	for key, n := range map[string]int{"api:alice": 4, "api:bob": 2, DefaultSessionKey: 2} {
		if sess, _ := h.sessions.GetOrCreate(key); len(sess.Messages) != n {
			t.Errorf("session %s has %d messages, want %d", key, len(sess.Messages), n)
		}
	}
}

func TestSessionTurnsAreSerialized(t *testing.T) {
	h := newHarness(t, nil)
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Go(func() {
			r := do(t, h.srv, http.MethodPost, "/v1/chat/completions", "", chatBody(fmt.Sprintf("m%d", i), "same"))
			if r.status != http.StatusOK {
				t.Errorf("status = %d %s", r.status, r.body)
			}
		})
	}
	wg.Wait()
	// Each turn saw the ones before it: the session holds every exchange.
	if sess, _ := h.sessions.GetOrCreate("api:same"); len(sess.Messages) != 10 {
		t.Errorf("session has %d messages, want 10", len(sess.Messages))
	}
}

func TestChatCompletionMessageForms(t *testing.T) {
	h := newHarness(t, nil)
	tests := []struct {
		name, body, want string
	}{
		{"content parts", `{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`, "echo a b (1)"},
		{"history is ignored", `{"messages":[{"role":"system","content":"be terse"},{"role":"user","content":"old"},{"role":"assistant","content":"x"},{"role":"user","content":"new"}],"session_id":"forms"}`, "echo new (1)"},
		{"unknown fields ignored", `{"messages":[{"role":"user","content":"t"}],"temperature":0.5,"max_tokens":3,"session_id":"forms2"}`, "echo t (1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decodeCompletion(t, do(t, h.srv, http.MethodPost, "/v1/chat/completions", "", tt.body)).content(); got != tt.want {
				t.Errorf("content = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChatCompletionBadRequests(t *testing.T) {
	h := newHarness(t, nil)
	tests := []struct {
		name, contentType, body string
		status                  int
		message                 string
	}{
		{"invalid json", "application/json", `{"messages":`, 400, "Invalid JSON body"},
		{"not an object", "application/json", `[1]`, 400, "Invalid JSON body"},
		{"empty body", "application/json", ``, 400, "Invalid JSON body"},
		{"no messages", "application/json", `{"messages":[]}`, 400, "user message"},
		{"missing messages", "application/json", `{}`, 400, "user message"},
		{"last not user", "application/json", `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`, 400, "role 'user'"},
		{"empty content", "application/json", chatBody("   ", ""), 400, "must not be empty"},
		{"null content", "application/json", `{"messages":[{"role":"user","content":null}]}`, 400, "must not be empty"},
		{"bad content", "application/json", `{"messages":[{"role":"user","content":5}]}`, 400, "Invalid content format"},
		{"image part", "application/json", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`, 400, "image_url"},
		{"wrong model", "application/json", `{"model":"gpt-4o","messages":[{"role":"user","content":"a"}]}`, 400, "Only configured model 'test-model'"},
		{"stream not bool", "application/json", `{"stream":"yes","messages":[{"role":"user","content":"a"}]}`, 400, "Invalid type for stream"},
		{"session id not string", "application/json", `{"session_id":5,"messages":[{"role":"user","content":"a"}]}`, 400, "session_id"},
		{"session id too long", "application/json", chatBody("a", strings.Repeat("x", maxSessionID+1)), 400, "session_id"},
		{"session id control char", "application/json", chatBody("a", "a\nb"), 400, "session_id"},
		{"multipart", "multipart/form-data; boundary=x", "--x--", 400, "multipart"},
		{"text plain", "text/plain", "hi", 400, "application/json"},
		{"too large", "application/json", `{"messages":[{"role":"user","content":"` + strings.Repeat("a", MaxBodyBytes) + `"}]}`, 413, "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/chat/completions", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", tt.contentType)
			resp, err := h.srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			wantError(t, response{resp.StatusCode, resp.Header, string(body)}, tt.status, errInvalidRequest, tt.message)
		})
	}
}

func TestAuth(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.APIKey = "s3cret" })
	tests := []struct {
		name, method, path, auth, body string
		status                         int
		message                        string
	}{
		{"health is open", http.MethodGet, "/health", "", "", 200, ""},
		{"models missing", http.MethodGet, "/v1/models", "", "", 401, "Missing Authorization"},
		{"models wrong scheme", http.MethodGet, "/v1/models", "Basic czNjcmV0", "", 401, "Missing Authorization"},
		{"models invalid", http.MethodGet, "/v1/models", "Bearer nope", "", 401, "Invalid API key"},
		{"models prefix of key", http.MethodGet, "/v1/models", "Bearer s3cre", "", 401, "Invalid API key"},
		{"models valid", http.MethodGet, "/v1/models", "Bearer s3cret", "", 200, ""},
		{"chat missing", http.MethodPost, "/v1/chat/completions", "", chatBody("hi", ""), 401, "Missing Authorization"},
		{"chat invalid", http.MethodPost, "/v1/chat/completions", "Bearer S3CRET", chatBody("hi", ""), 401, "Invalid API key"},
		{"chat valid", http.MethodPost, "/v1/chat/completions", "Bearer s3cret", chatBody("hi", ""), 200, ""},
		{"unknown route needs auth", http.MethodGet, "/nope", "", "", 401, "Missing Authorization"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := do(t, h.srv, tt.method, tt.path, tt.auth, tt.body)
			if tt.status != http.StatusOK {
				wantError(t, r, tt.status, errInvalidRequest, tt.message)
				if r.header.Get("WWW-Authenticate") != "Bearer" {
					t.Errorf("WWW-Authenticate = %q", r.header.Get("WWW-Authenticate"))
				}
				return
			}
			if r.status != http.StatusOK {
				t.Errorf("status = %d %s", r.status, r.body)
			}
		})
	}
	// A rejected request never reaches the agent.
	if sess, _ := h.sessions.GetOrCreate(DefaultSessionKey); len(sess.Messages) != 2 {
		t.Errorf("default session has %d messages, want only the authorized turn", len(sess.Messages))
	}
}

func TestNoKeyIgnoresAuthorizationHeader(t *testing.T) {
	h := newHarness(t, nil)
	if r := do(t, h.srv, http.MethodGet, "/v1/models", "Bearer anything", ""); r.status != http.StatusOK {
		t.Errorf("status = %d %s", r.status, r.body)
	}
}

func TestTurnFailures(t *testing.T) {
	tests := []struct {
		name    string
		turn    TurnFunc
		status  int
		message string
	}{
		{"provider error", func(context.Context, bus.InboundMessage) (agent.Reply, error) {
			return agent.Reply{}, errors.New("upstream exploded with secret detail")
		}, 500, "Internal server error"},
		{"timeout", func(ctx context.Context, _ bus.InboundMessage) (agent.Reply, error) {
			<-ctx.Done()
			return agent.Reply{}, ctx.Err()
		}, 504, "timed out"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, srv := stubServer(t, 50*time.Millisecond, tt.turn)
			r := do(t, srv, http.MethodPost, "/v1/chat/completions", "", chatBody("hi", ""))
			wantError(t, r, tt.status, errServer, tt.message)
			if strings.Contains(r.body, "secret detail") {
				t.Errorf("internal error leaked: %s", r.body)
			}
		})
	}
}

func TestTurnMessage(t *testing.T) {
	got := make(chan bus.InboundMessage, 1)
	_, srv := stubServer(t, time.Second, func(_ context.Context, msg bus.InboundMessage) (agent.Reply, error) {
		got <- msg
		return agent.Reply{Content: "ok"}, nil
	})
	decodeCompletion(t, do(t, srv, http.MethodPost, "/v1/chat/completions", "", chatBody("hello", "s-1")))
	msg := <-got
	if msg.Channel != "api" || msg.ChatID != "default" || msg.SenderID != "user" || msg.Content != "hello" ||
		msg.SessionKey() != "api:s-1" || msg.Timestamp.IsZero() {
		t.Errorf("message = %+v", msg)
	}
}

// readEvents reads SSE data payloads and comments until EOF.
func readEvents(t *testing.T, body io.Reader) (data []string, comments int) {
	t.Helper()
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		case strings.HasPrefix(line, ":"):
			comments++
		case line == "":
		default:
			t.Errorf("unexpected SSE line %q", line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return data, comments
}

func postStream(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := srv.Client().Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestStreaming(t *testing.T) {
	h := newHarness(t, nil)
	resp := postStream(t, h.srv, `{"stream":true,"session_id":"s","messages":[{"role":"user","content":"please use the tool"}]}`)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("status %d, headers %v", resp.StatusCode, resp.Header)
	}
	data, _ := readEvents(t, resp.Body)
	if len(data) != 3 || data[2] != "[DONE]" {
		t.Fatalf("events = %q", data)
	}
	var first, last completionBody
	if err := json.Unmarshal([]byte(data[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(data[1]), &last); err != nil {
		t.Fatal(err)
	}
	if first.Object != "chat.completion.chunk" || first.Model != testModel || !strings.HasPrefix(first.ID, "chatcmpl-") || first.ID != last.ID {
		t.Errorf("chunk = %+v / %+v", first, last)
	}
	if d := first.Choices[0].Delta; d["content"] != "tool said 42" || d["role"] != "assistant" || first.Choices[0].FinishReason != nil {
		t.Errorf("first chunk choice = %+v", first.Choices[0])
	}
	if !strings.Contains(data[0], `"finish_reason":null`) || !strings.Contains(data[1], `"delta":{}`) {
		t.Errorf("chunks = %s / %s", data[0], data[1])
	}
	if fr := last.Choices[0].FinishReason; fr == nil || *fr != "stop" || len(last.Choices[0].Delta) != 0 {
		t.Errorf("last chunk choice = %+v", last.Choices[0])
	}
	if sess, _ := h.sessions.GetOrCreate("api:s"); len(sess.Messages) != 4 {
		t.Errorf("streamed turn not saved: %d messages", len(sess.Messages))
	}
}

func TestStreamingKeepAlive(t *testing.T) {
	release := make(chan struct{})
	s, srv := stubServer(t, 5*time.Second, func(ctx context.Context, _ bus.InboundMessage) (agent.Reply, error) {
		select {
		case <-release:
			return agent.Reply{Content: "late"}, nil
		case <-ctx.Done():
			return agent.Reply{}, ctx.Err()
		}
	})
	s.keepAlive = 10 * time.Millisecond
	resp := postStream(t, srv, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	// Headers arrive before the turn ends.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	data, comments := readEvents(t, resp.Body)
	if comments == 0 {
		t.Error("no keep-alive comments while the turn ran")
	}
	if len(data) != 3 || !strings.Contains(data[0], `"content":"late"`) || data[2] != "[DONE]" {
		t.Errorf("events = %q", data)
	}
}

func TestStreamingFailureEndsWithoutDone(t *testing.T) {
	tests := []struct {
		name string
		turn TurnFunc
	}{
		{"error", func(context.Context, bus.InboundMessage) (agent.Reply, error) {
			return agent.Reply{}, errors.New("boom")
		}},
		{"timeout", func(ctx context.Context, _ bus.InboundMessage) (agent.Reply, error) {
			<-ctx.Done()
			return agent.Reply{}, ctx.Err()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, srv := stubServer(t, 50*time.Millisecond, tt.turn)
			resp := postStream(t, srv, `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if data, _ := readEvents(t, resp.Body); len(data) != 0 {
				t.Errorf("events = %q, want none", data)
			}
		})
	}
}

func TestClientDisconnectCancelsTurn(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			started, canceled := make(chan struct{}), make(chan struct{})
			_, srv := stubServer(t, 10*time.Second, func(ctx context.Context, _ bus.InboundMessage) (agent.Reply, error) {
				close(started)
				<-ctx.Done()
				close(canceled)
				return agent.Reply{}, ctx.Err()
			})
			ctx, cancel := context.WithCancel(context.Background())
			body := fmt.Sprintf(`{"stream":%t,"messages":[{"role":"user","content":"hi"}]}`, stream)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			errc := make(chan error, 1)
			go func() {
				resp, err := srv.Client().Do(req)
				if err == nil {
					_, err = io.ReadAll(resp.Body)
					resp.Body.Close()
				}
				errc <- err
			}()
			<-started
			cancel()
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("turn not canceled after the client went away")
			}
			<-errc
		})
	}
}

func TestStartAndShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s, err := New(Config{Host: "127.0.0.1", Port: 0, Model: testModel, Timeout: 5 * time.Second,
		Turn: func(context.Context, bus.InboundMessage) (agent.Reply, error) {
			close(started)
			<-release
			return agent.Reply{Content: "finished"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := s.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(context.Background()); err == nil {
		t.Error("second Start succeeded")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	type result struct {
		status int
		body   string
		err    error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := client.Post("http://"+addr+"/v1/chat/completions", "application/json", strings.NewReader(chatBody("hi", "")))
		if err != nil {
			res <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		res <- result{status: resp.StatusCode, body: string(b)}
	}()
	<-started

	// Shutdown waits for the request in flight.
	shut := make(chan error, 1)
	go func() { shut <- s.Shutdown(context.Background()) }()
	select {
	case err := <-shut:
		t.Fatalf("Shutdown returned with a request in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-shut; err != nil {
		t.Errorf("Shutdown = %v", err)
	}
	if r := <-res; r.err != nil || r.status != http.StatusOK || !strings.Contains(r.body, "finished") {
		t.Errorf("in-flight request = %+v", r)
	}
	if _, err := client.Get("http://" + addr + "/health"); err == nil {
		t.Error("still serving after Shutdown")
	}
}

func TestShutdownBaseContextCancelsTurns(t *testing.T) {
	started := make(chan struct{})
	s, err := New(Config{Host: "127.0.0.1", Model: testModel, Timeout: time.Minute,
		Turn: func(ctx context.Context, _ bus.InboundMessage) (agent.Reply, error) {
			close(started)
			<-ctx.Done()
			return agent.Reply{}, ctx.Err()
		}})
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	addr, err := s.Start(base)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		resp, err := http.Post("http://"+addr+"/v1/chat/completions", "application/json", strings.NewReader(chatBody("hi", "")))
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	cancel()
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown = %v", err)
	}
}

func TestStartPortInUse(t *testing.T) {
	first, err := New(Config{Host: "127.0.0.1", Model: testModel, Timeout: time.Second,
		Turn: func(context.Context, bus.InboundMessage) (agent.Reply, error) { return agent.Reply{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	addr, err := first.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Shutdown(context.Background())
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := New(Config{Host: "127.0.0.1", Port: port, Model: testModel, Timeout: time.Second, Turn: first.cfg.Turn})
	if _, err := second.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "api:") {
		t.Errorf("Start on a taken port = %v", err)
	}
}
