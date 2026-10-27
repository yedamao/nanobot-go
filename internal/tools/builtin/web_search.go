package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yedamao/nanobot-go/internal/tools"
)

// The DeepSeek Search and Fetch APIs are client-side tools: plain JSON
// POSTs billed per successful request. They are unrelated to the
// provider-hosted search of DeepSeek models; nothing here is injected
// into a chat completion.
const (
	DeepSeekSearchURL = "https://api.deepseek.com/v1/tools/search"
	DeepSeekFetchURL  = "https://api.deepseek.com/v1/tools/fetch"
	// The Search API accepts 1-128 Unicode characters per query and 1-50
	// results.
	deepseekMaxQueryChars = 128
	deepseekMaxResults    = 50
	// summary is documented at 600-800 characters but has been seen near
	// 4,500, so each result is capped.
	deepseekMaxSummaryChars = 500
	deepseekErrorDetailMax  = 300
	// The Fetch API accepts URLs of at most 8192 characters.
	deepseekMaxFetchURL = 8192
	// One retry after a transient status; this does not coordinate
	// account-wide QPS.
	deepseekRetryDelay      = time.Second
	defaultWebSearchResults = 5
	deepseekMaxResponse     = 4 << 20
)

// WebSearchConfig configures the web_search tool.
type WebSearchConfig struct {
	// Provider must be "deepseek" for the tool to be registered.
	Provider string
	// APIKey is the resolved DeepSeek key; empty fails every call with a
	// hint on where to set it.
	APIKey string
	// MaxResults is the default count (default 5, at most 50).
	MaxResults int
	// Timeout bounds each request (default 30s).
	Timeout   time.Duration
	UserAgent string
	// URL overrides DeepSeekSearchURL (tests).
	URL string
	// Client overrides the HTTP client (tests).
	Client *http.Client
}

// Enabled reports whether cfg selects a supported search provider.
func (cfg WebSearchConfig) Enabled() bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Provider), "deepseek")
}

// WebSearch implements web_search with the DeepSeek Search API.
type WebSearch struct {
	cfg        WebSearchConfig
	client     *http.Client
	retryDelay time.Duration
}

// NewWebSearch returns the web_search tool.
func NewWebSearch(cfg WebSearchConfig) *WebSearch {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultWebTimeout
	}
	if cfg.MaxResults <= 0 {
		cfg.MaxResults = defaultWebSearchResults
	}
	cfg.MaxResults = min(cfg.MaxResults, deepseekMaxResults)
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultWebUserAgent
	}
	if cfg.URL == "" {
		cfg.URL = DeepSeekSearchURL
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &WebSearch{cfg: cfg, client: client, retryDelay: deepseekRetryDelay}
}

func (*WebSearch) Name() string { return "web_search" }
func (*WebSearch) Description() string {
	return "Search the web for titles, URLs, and snippets. Use web_fetch to read a specific page."
}
func (*WebSearch) ReadOnly() bool { return true }
func (w *WebSearch) Parameters() map[string]any {
	return tools.Object(map[string]any{
		"query": tools.String(fmt.Sprintf("Search query (at most %d characters).", deepseekMaxQueryChars)),
		"count": tools.Integer(fmt.Sprintf("Number of results, 1-%d (default %d).", deepseekMaxResults, w.cfg.MaxResults)),
	}, "query")
}

func (w *WebSearch) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	args, err := tools.DecodeArgs[struct {
		Query string `json:"query"`
		Count int    `json:"count"`
	}](raw)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if w.cfg.APIKey == "" {
		return tools.Errorf("DeepSeek search requires an API key. Set tools.web.search.apiKey or providers.deepseek.apiKey.")
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return tools.Errorf("DeepSeek search requires a non-blank query.")
	}
	if utf8.RuneCountInString(query) > deepseekMaxQueryChars {
		return tools.Errorf("DeepSeek search accepts at most %d Unicode characters. "+
			"Shorten or split the query while preserving its search operators.", deepseekMaxQueryChars)
	}
	n := args.Count
	if n <= 0 {
		n = w.cfg.MaxResults
	}
	n = min(max(n, 1), deepseekMaxResults)
	payload := map[string]any{
		"query": query,
		"count": n,
		// Keep full page bodies out of the model's context; summaries suffice.
		"need_content": false,
	}
	body, fail := deepseekPost(ctx, deepseekCall{
		client: w.client, url: w.cfg.URL, key: w.cfg.APIKey, userAgent: w.cfg.UserAgent,
		retryDelay: w.retryDelay, what: "search", keyHint: "tools.web.search.apiKey",
		timeoutHint: "tools.web.search.timeout", tool: "web_search",
	}, payload)
	if fail != nil {
		return *fail
	}
	var data struct {
		Results json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		if errors.As(err, new(*json.UnmarshalTypeError)) {
			return tools.Errorf("DeepSeek search returned an unexpected response shape.")
		}
		return tools.Errorf("DeepSeek search returned a response that was not valid JSON.")
	}
	var results []json.RawMessage
	if len(data.Results) > 0 && string(data.Results) != "null" {
		if err := json.Unmarshal(data.Results, &results); err != nil {
			return tools.Errorf("DeepSeek search returned an unexpected response shape.")
		}
	}
	items := make([]searchItem, 0, len(results))
	for _, r := range results {
		var obj map[string]any
		if json.Unmarshal(r, &obj) != nil || obj == nil {
			continue
		}
		// Prefer the long summary written for models; snippet is a
		// frontend-oriented short abstract. Both are trimmed, so a
		// whitespace-only summary falls through to the snippet.
		content := apiText(obj["summary"])
		if content == "" {
			content = apiText(obj["snippet"])
		}
		content = truncateRunes(content, deepseekMaxSummaryChars)
		if date, ok := obj["date"].(string); ok && date != "" {
			if content != "" {
				content = date + "\n" + content
			} else {
				content = date
			}
		}
		items = append(items, searchItem{title: apiText(obj["title"]), url: apiText(obj["url"]), content: content})
	}
	return tools.Text(formatSearchResults(query, items, n))
}

type searchItem struct{ title, url, content string }

// formatSearchResults renders results as nanobot's _format_results does.
func formatSearchResults(query string, items []searchItem, n int) string {
	if len(items) == 0 {
		return "No results for: " + query
	}
	lines := []string{"Results for: " + query + "\n"}
	for i, it := range items[:min(n, len(items))] {
		lines = append(lines, fmt.Sprintf("%d. %s\n   %s", i+1, normalizeText(stripTags(it.title)), it.url))
		if s := normalizeText(stripTags(it.content)); s != "" {
			lines = append(lines, "   "+s)
		}
	}
	return strings.Join(lines, "\n")
}

// deepseekCall describes one DeepSeek tools API request.
type deepseekCall struct {
	client      *http.Client
	url, key    string
	userAgent   string
	retryDelay  time.Duration
	what        string // "search" or "fetch", for messages
	keyHint     string // config field for the key
	timeoutHint string // config field for the timeout, if any
	tool        string // tool name, for the rate limit message
}

// deepseekPost POSTs payload, retrying once after a transient status, and
// returns the body of a 2xx answer or the error result to return.
func deepseekPost(ctx context.Context, c deepseekCall, payload any) ([]byte, *tools.Result) {
	fail := func(format string, args ...any) ([]byte, *tools.Result) {
		r := tools.Errorf("DeepSeek "+c.what+" "+format, args...)
		return nil, &r
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return fail("failed: %v", err)
	}
	var (
		status int
		body   []byte
	)
	for attempt := 0; attempt < 2; attempt++ {
		status, body, err = deepseekDo(ctx, c, reqBody)
		if err != nil {
			if isTimeout(err) && c.timeoutHint != "" {
				return fail("timed out. Retry later or raise %s.", c.timeoutHint)
			}
			if isTimeout(err) {
				return fail("timed out. Retry later.")
			}
			return fail("failed: %v", err)
		}
		if !deepseekRetryable(status) || attempt == 1 {
			break
		}
		select {
		case <-ctx.Done():
			return fail("failed: %v", ctx.Err())
		case <-time.After(c.retryDelay):
		}
	}
	switch {
	case status == http.StatusUnauthorized:
		return fail("rejected the API key (HTTP 401). Set a valid key in %s or providers.deepseek.apiKey.", c.keyHint)
	case status == http.StatusPaymentRequired:
		r := tools.Errorf("DeepSeek account has insufficient balance (HTTP 402). Top up at https://platform.deepseek.com/top_up and retry.")
		return nil, &r
	case status == http.StatusTooManyRequests:
		return fail("rate limited after retry. The account has a QPS limit; retry later or reduce the frequency of concurrent %s calls.", c.tool)
	case deepseekRetryable(status):
		return fail("failed (%d) after retry. Retry later.", status)
	case status < 200 || status > 299:
		if detail := deepseekErrorDetail(body); detail != "" {
			return fail("failed (%d): %s", status, detail)
		}
		return fail("failed (%d)", status)
	}
	return body, nil
}

func deepseekDo(ctx context.Context, c deepseekCall, reqBody []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(reqBody))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, scrubURLError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, deepseekMaxResponse))
	if err != nil {
		return 0, nil, scrubURLError(err)
	}
	return resp.StatusCode, body, nil
}

// scrubURLError drops the *url.Error wrapper ("Post \"...\": ") so only
// the cause reaches the model.
func scrubURLError(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// deepseekRetryable reports transient statuses: 408, 429 and 5xx. Every
// other 4xx needs a new request.
func deepseekRetryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// deepseekErrorDetail reads error.message from the OpenAI-compatible error
// envelope as plain text: the upstream body is untrusted and is echoed to
// the model, so it is tag-stripped and whitespace-collapsed.
func deepseekErrorDetail(body []byte) string {
	var env struct {
		Error struct {
			Message any `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	msg, ok := env.Error.Message.(string)
	if !ok {
		return ""
	}
	return truncateRunes(normalizeText(stripTags(msg)), deepseekErrorDetailMax)
}

// apiText coerces an optional API string field, tolerating null and
// non-strings.
func apiText(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

var (
	reStripScript = regexp.MustCompile(`(?i)<script[\s\S]*?</script>`)
	reStripStyle  = regexp.MustCompile(`(?i)<style[\s\S]*?</style>`)
	reStripTag    = regexp.MustCompile(`<[^>]+>`)
	reHSpaces     = regexp.MustCompile(`[ \t]+`)
)

// stripTags removes HTML tags and decodes entities (nanobot's _strip_tags).
func stripTags(s string) string {
	s = reStripScript.ReplaceAllString(s, "")
	s = reStripStyle.ReplaceAllString(s, "")
	s = reStripTag.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}

// normalizeText collapses spaces and blank lines (nanobot's _normalize).
func normalizeText(s string) string {
	s = reHSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(reBlankLines.ReplaceAllString(s, "\n\n"))
}
