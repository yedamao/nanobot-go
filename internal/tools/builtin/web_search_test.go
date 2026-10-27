package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/tools"
)

// deepseekServer answers every request with handler and records the
// decoded request bodies.
func deepseekServer(t *testing.T, handler func(w http.ResponseWriter, attempt int)) (*httptest.Server, *[]map[string]any, *atomic.Int32) {
	t.Helper()
	var bodies []map[string]any
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		bodies = append(bodies, body)
		handler(w, n)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies, &calls
}

func newTestSearch(url, key string, maxResults int) *WebSearch {
	s := NewWebSearch(WebSearchConfig{Provider: "deepseek", APIKey: key, MaxResults: maxResults, URL: url, Timeout: 5 * time.Second})
	s.retryDelay = time.Millisecond
	return s
}

func runSearch(t *testing.T, s *WebSearch, args string) tools.Result {
	t.Helper()
	return s.Execute(context.Background(), json.RawMessage(args))
}

func TestWebSearchFormatsResults(t *testing.T) {
	long := strings.Repeat("é", 600)
	srv, bodies, _ := deepseekServer(t, func(w http.ResponseWriter, _ int) {
		fmt.Fprintf(w, `{"results":[
		  {"title":"<b>Go</b> &amp; you","url":"https://go.dev","summary":"  long   summary  ","snippet":"short","date":"2026-10-01T00:00:00Z"},
		  {"title":"Second","url":"https://b.example","summary":"   ","snippet":"only snippet","date":null},
		  {"title":"Third","url":"https://c.example","summary":%q},
		  "not an object",
		  {"title":"Fourth","url":"https://d.example"}
		]}`, long)
	})
	s := newTestSearch(srv.URL, "sk-test", 5)
	res := runSearch(t, s, `{"query":"  golang  "}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	want := "Results for: golang\n\n" +
		"1. Go & you\n   https://go.dev\n   2026-10-01T00:00:00Z\nlong summary\n" +
		"2. Second\n   https://b.example\n   only snippet\n" +
		"3. Third\n   https://c.example\n   " + strings.Repeat("é", 500) + "\n" +
		"4. Fourth\n   https://d.example"
	if res.Content != want {
		t.Errorf("content =\n%s\nwant\n%s", res.Content, want)
	}
	b := (*bodies)[0]
	if b["query"] != "golang" || b["count"] != float64(5) || b["need_content"] != false {
		t.Errorf("request body = %v", b)
	}
}

func TestWebSearchCount(t *testing.T) {
	srv, bodies, _ := deepseekServer(t, func(w http.ResponseWriter, _ int) {
		fmt.Fprint(w, `{"results":[{"title":"A","url":"u1"},{"title":"B","url":"u2"},{"title":"C","url":"u3"}]}`)
	})
	s := newTestSearch(srv.URL, "sk-test", 7)
	for _, tc := range []struct {
		args string
		want float64
	}{
		{`{"query":"q"}`, 7},
		{`{"query":"q","count":2}`, 2},
		{`{"query":"q","count":500}`, 50},
		{`{"query":"q","count":-3}`, 7},
	} {
		*bodies = nil
		res := runSearch(t, s, tc.args)
		if res.IsError {
			t.Fatalf("%s: %s", tc.args, res.Content)
		}
		if got := (*bodies)[0]["count"]; got != tc.want {
			t.Errorf("%s: count = %v, want %v", tc.args, got, tc.want)
		}
	}
	// Results beyond count are dropped.
	res := runSearch(t, s, `{"query":"q","count":2}`)
	if strings.Contains(res.Content, "3. C") || !strings.Contains(res.Content, "2. B") {
		t.Errorf("count 2 content = %s", res.Content)
	}
	// MaxResults is clamped to 50 and defaults to 5.
	if s := NewWebSearch(WebSearchConfig{MaxResults: 99}); s.cfg.MaxResults != 50 {
		t.Errorf("MaxResults 99 clamped to %d", s.cfg.MaxResults)
	}
	if s := NewWebSearch(WebSearchConfig{}); s.cfg.MaxResults != 5 {
		t.Errorf("default MaxResults = %d", s.cfg.MaxResults)
	}
}

func TestWebSearchNoResults(t *testing.T) {
	for _, body := range []string{`{"results":[]}`, `{"results":null}`, `{}`} {
		srv, _, _ := deepseekServer(t, func(w http.ResponseWriter, _ int) { fmt.Fprint(w, body) })
		res := runSearch(t, newTestSearch(srv.URL, "sk-test", 5), `{"query":"nothing"}`)
		if res.IsError || res.Content != "No results for: nothing" {
			t.Errorf("%s: %+v", body, res)
		}
	}
	for _, body := range []string{`{"results":"x"}`, `[]`, `not json`} {
		srv, _, _ := deepseekServer(t, func(w http.ResponseWriter, _ int) { fmt.Fprint(w, body) })
		res := runSearch(t, newTestSearch(srv.URL, "sk-test", 5), `{"query":"q"}`)
		if !res.IsError || !strings.Contains(res.Content, "DeepSeek search returned") {
			t.Errorf("%s: %+v", body, res)
		}
	}
}

func TestWebSearchHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		status    int
		body      string
		want      string
		wantCalls int32
	}{
		{401, `{}`, "DeepSeek search rejected the API key (HTTP 401). Set a valid key in tools.web.search.apiKey", 1},
		{402, `{}`, "DeepSeek account has insufficient balance (HTTP 402). Top up at https://platform.deepseek.com/top_up and retry.", 1},
		{429, `{}`, "DeepSeek search rate limited after retry.", 2},
		{503, `{}`, "DeepSeek search failed (503) after retry. Retry later.", 2},
		{400, `{"error":{"message":"bad <i>query</i>   here","type":"invalid_request_error"}}`, "DeepSeek search failed (400): bad query here", 1},
		{400, `oops`, "DeepSeek search failed (400)", 1},
	} {
		srv, _, calls := deepseekServer(t, func(w http.ResponseWriter, _ int) {
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		})
		res := runSearch(t, newTestSearch(srv.URL, "sk-test", 5), `{"query":"q"}`)
		if !res.IsError || !strings.Contains(res.Content, tc.want) {
			t.Errorf("HTTP %d: content = %q, want %q", tc.status, res.Content, tc.want)
		}
		if got := calls.Load(); got != tc.wantCalls {
			t.Errorf("HTTP %d: %d calls, want %d", tc.status, got, tc.wantCalls)
		}
	}
}

func TestWebSearchRetriesOnce(t *testing.T) {
	srv, _, calls := deepseekServer(t, func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"results":[{"title":"OK","url":"https://ok.example","snippet":"fine"}]}`)
	})
	res := runSearch(t, newTestSearch(srv.URL, "sk-test", 5), `{"query":"q"}`)
	if res.IsError || !strings.Contains(res.Content, "1. OK") {
		t.Fatalf("content = %+v", res)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestWebSearchRejectsBadInput(t *testing.T) {
	srv, _, calls := deepseekServer(t, func(w http.ResponseWriter, _ int) { fmt.Fprint(w, `{"results":[]}`) })
	s := newTestSearch(srv.URL, "sk-test", 5)
	ok := strings.Repeat("界", 128)
	if res := runSearch(t, s, fmt.Sprintf(`{"query":%q}`, ok)); res.IsError {
		t.Errorf("128 runes rejected: %s", res.Content)
	}
	for _, tc := range []struct{ args, want string }{
		{fmt.Sprintf(`{"query":%q}`, ok+"界"), "at most 128 Unicode characters"},
		{`{"query":"   "}`, "requires a non-blank query"},
		{`{"query":""}`, "requires a non-blank query"},
	} {
		res := runSearch(t, s, tc.args)
		if !res.IsError || !strings.Contains(res.Content, tc.want) {
			t.Errorf("%s: %+v", tc.args, res)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want only the valid query", calls.Load())
	}

	res := runSearch(t, newTestSearch(srv.URL, "", 5), `{"query":"q"}`)
	if !res.IsError || !strings.Contains(res.Content, "tools.web.search.apiKey or providers.deepseek.apiKey") {
		t.Errorf("missing key: %+v", res)
	}
}

func TestWebSearchTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	s := NewWebSearch(WebSearchConfig{Provider: "deepseek", APIKey: "k", URL: srv.URL, Timeout: 50 * time.Millisecond})
	res := runSearch(t, s, `{"query":"q"}`)
	if !res.IsError || !strings.Contains(res.Content, "timed out") || strings.Contains(res.Content, srv.URL) {
		t.Errorf("timeout: %+v", res)
	}
}

func TestRegisterAllWebSearch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disabled bool
		provider string
		want     bool
	}{
		{"deepseek", false, "deepseek", true},
		{"case-insensitive", false, "DeepSeek", true},
		{"empty", false, "", false},
		{"unknown", false, "brave", false},
		{"web disabled", true, "deepseek", false},
	} {
		r := tools.NewRegistry()
		err := RegisterAll(r, Config{Workspace: newWS(t), Exec: ExecConfig{Disabled: true},
			Web: WebFetchConfig{Disabled: tc.disabled}, Search: WebSearchConfig{Provider: tc.provider}})
		if err != nil {
			t.Fatal(err)
		}
		_, gotSearch := r.Get("web_search")
		_, gotFetch := r.Get("web_fetch")
		if gotSearch != tc.want || gotFetch == tc.disabled {
			t.Errorf("%s: web_search %v web_fetch %v", tc.name, gotSearch, gotFetch)
		}
	}
}

func TestWebFetchDeepSeek(t *testing.T) {
	srv, bodies, _ := deepseekServer(t, func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"title":" Example ","content":"# Heading\n\nBody text."}`)
	})
	f := NewWebFetch(WebFetchConfig{Provider: "deepseek", DeepSeekAPIKey: "sk-test", DeepSeekURL: srv.URL})
	f.retryDelay = time.Millisecond
	res := f.Execute(context.Background(), json.RawMessage(`{"url":"https://example.com/a?b=1"}`))
	if res.IsError {
		t.Fatalf("error: %s", res.Content)
	}
	for _, want := range []string{"URL: https://example.com/a?b=1", "Title: Example", untrustedNote, "# Heading\n\nBody text."} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q:\n%s", want, res.Content)
		}
	}
	if got := (*bodies)[len(*bodies)-1]["url"]; got != "https://example.com/a?b=1" {
		t.Errorf("request url = %v", got)
	}

	for _, tc := range []struct{ url, want string }{
		{"ftp://example.com/x", "only http and https"},
		{"https://user:pw@example.com/", "credentials"},
		{"https://example.com/" + strings.Repeat("a", 8200), "longer than 8192"},
	} {
		res := f.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"url":%q}`, tc.url)))
		if !res.IsError || !strings.Contains(res.Content, tc.want) {
			t.Errorf("%.40s: %+v", tc.url, res)
		}
	}

	nokey := NewWebFetch(WebFetchConfig{Provider: "deepseek", DeepSeekURL: srv.URL})
	res = nokey.Execute(context.Background(), json.RawMessage(`{"url":"https://example.com/"}`))
	if !res.IsError || !strings.Contains(res.Content, "requires an API key") {
		t.Errorf("missing key: %+v", res)
	}
}

func TestWebFetchDeepSeekErrors(t *testing.T) {
	srv, _, _ := deepseekServer(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"message":"no usable content"}}`)
	})
	f := NewWebFetch(WebFetchConfig{Provider: "deepseek", DeepSeekAPIKey: "sk-test", DeepSeekURL: srv.URL})
	res := f.Execute(context.Background(), json.RawMessage(`{"url":"https://example.com/"}`))
	if !res.IsError || res.Content != "Error: DeepSeek fetch failed (404): no usable content" {
		t.Errorf("content = %q", res.Content)
	}
}
