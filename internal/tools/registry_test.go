package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// fakeTool records the arguments it receives and returns a canned result.
type fakeTool struct {
	name   string
	params map[string]any
	run    func(ctx context.Context, args json.RawMessage) Result
}

func (f *fakeTool) Name() string               { return f.name }
func (f *fakeTool) Description() string        { return "fake " + f.name }
func (f *fakeTool) Parameters() map[string]any { return f.params }
func (f *fakeTool) ReadOnly() bool             { return true }
func (f *fakeTool) Execute(ctx context.Context, args json.RawMessage) Result {
	if f.run != nil {
		return f.run(ctx, args)
	}
	return Text(string(args))
}

func echoSchema() map[string]any {
	return Object(map[string]any{
		"path":   String("file path"),
		"limit":  Integer("max lines"),
		"ratio":  map[string]any{"type": "number"},
		"all":    Boolean("all"),
		"tags":   map[string]any{"type": "array"},
		"mode":   map[string]any{"type": "string", "enum": []any{"fast", "slow"}},
		"nested": map[string]any{"type": "object"},
	}, "path")
}

func newTestRegistry(t *testing.T, opts ...Option) *Registry {
	t.Helper()
	r := NewRegistry(opts...)
	for _, tool := range []Tool{
		&fakeTool{name: "read_file", params: echoSchema()},
		&fakeTool{name: "write_file", params: echoSchema()},
		&fakeTool{name: "mcp_github_search", params: Object(nil)},
		&fakeTool{name: "exec", params: Object(map[string]any{"command": String("cmd")}, "command")},
	} {
		if err := r.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestRegisterRejectsDuplicatesAndBadNames(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Register(&fakeTool{name: "exec"}); err == nil {
		t.Error("expected duplicate error")
	}
	for _, bad := range []string{"", "has space", strings.Repeat("x", 65), "dot.name"} {
		if err := r.Register(&fakeTool{name: bad}); err == nil {
			t.Errorf("expected error for name %q", bad)
		}
	}
	if err := r.Register(nil); err == nil {
		t.Error("expected error for nil tool")
	}
}

func TestDefinitionsStableOrder(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Register(&fakeTool{name: "mcp_a_tool", params: Object(nil)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&fakeTool{name: "edit_file", params: Object(nil)}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range r.Definitions() {
		got = append(got, d.Name)
	}
	want := "edit_file,exec,read_file,write_file,mcp_a_tool,mcp_github_search"
	if strings.Join(got, ",") != want {
		t.Errorf("order = %v, want %s", got, want)
	}

	r.Unregister("exec")
	if n := len(r.Definitions()); n != 5 {
		t.Errorf("after unregister got %d definitions, want 5", n)
	}
	if _, ok := r.Get("exec"); ok {
		t.Error("exec still registered")
	}
}

func TestRegisterEachAndUnregisterMany(t *testing.T) {
	r := newTestRegistry(t)
	errs := r.RegisterEach(
		&fakeTool{name: "mcp_x_a", params: Object(nil)},
		&fakeTool{name: "exec"}, // taken
		nil,
		&fakeTool{name: "bad name"},
		&fakeTool{name: "mcp_x_b", params: Object(nil)},
	)
	for i, wantErr := range []bool{false, true, true, true, false} {
		if (errs[i] != nil) != wantErr {
			t.Errorf("errs[%d] = %v, want error %t", i, errs[i], wantErr)
		}
	}
	if _, ok := r.Get("mcp_x_b"); !ok {
		t.Error("a rejected tool stopped later ones")
	}
	r.Unregister("mcp_x_a", "mcp_x_b", "never_registered")
	if got := strings.Join(r.Names(), ","); got != "exec,mcp_github_search,read_file,write_file" {
		t.Errorf("names after Unregister = %s", got)
	}
}

// TestBatchIsAtomic checks that readers see a batch fully or not at all.
func TestBatchIsAtomic(t *testing.T) {
	r := NewRegistry()
	batch := []Tool{
		&fakeTool{name: "mcp_s_a", params: Object(nil)},
		&fakeTool{name: "mcp_s_b", params: Object(nil)},
		&fakeTool{name: "mcp_s_c", params: Object(nil)},
	}
	names := []string{"mcp_s_a", "mcp_s_b", "mcp_s_c"}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < 200; i++ {
			r.RegisterEach(batch...)
			r.Unregister(names...)
		}
		close(stop)
	})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if n := len(r.Definitions()); n != 0 && n != len(batch) {
					t.Errorf("saw %d of %d batch tools", n, len(batch))
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestExecuteUnknownToolSuggests(t *testing.T) {
	r := newTestRegistry(t)
	tests := []struct {
		name, call, wantHint string
	}{
		{"case and punctuation", "ReadFile", "Did you mean 'read_file'"},
		{"typo", "red_file", "Did you mean 'read_file'"},
		{"far away", "teleport", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := r.Execute(context.Background(), tt.call, json.RawMessage(`{}`))
			if !res.IsError {
				t.Fatal("expected error")
			}
			if !strings.Contains(res.Content, "not found") || !strings.Contains(res.Content, "Available: exec,") {
				t.Errorf("content = %q", res.Content)
			}
			if tt.wantHint == "" && strings.Contains(res.Content, "Did you mean") {
				t.Errorf("unexpected suggestion: %q", res.Content)
			}
			if tt.wantHint != "" && !strings.Contains(res.Content, tt.wantHint) {
				t.Errorf("missing %q in %q", tt.wantHint, res.Content)
			}
			if strings.Count(res.Content, errorHint) != 1 {
				t.Errorf("error hint should appear once: %q", res.Content)
			}
		})
	}
}

func TestExecuteArgumentHandling(t *testing.T) {
	r := newTestRegistry(t)
	tests := []struct {
		name    string
		args    string
		wantErr string // substring; empty means success
		want    map[string]any
	}{
		{"plain object", `{"path":"a.txt"}`, "", map[string]any{"path": "a.txt"}},
		{"string encoded", `"{\"path\":\"b.txt\"}"`, "", map[string]any{"path": "b.txt"}},
		{"arguments wrapper", `{"arguments":{"path":"c.txt"}}`, "", map[string]any{"path": "c.txt"}},
		{"arguments wrapper string", `{"arguments":"{\"path\":\"d.txt\"}"}`, "", map[string]any{"path": "d.txt"}},
		{"coerce scalars", `{"path":"x","limit":"5","all":"true","ratio":"0.5"}`, "", map[string]any{"path": "x", "limit": float64(5), "all": true, "ratio": 0.5}},
		{"integral float", `{"path":"x","limit":3.0}`, "", map[string]any{"path": "x", "limit": float64(3)}},
		{"missing required", `{}`, "missing required parameter 'path'", nil},
		{"null required", `{"path":null}`, "missing required parameter 'path'", nil},
		{"empty args", ``, "missing required parameter 'path'", nil},
		{"wrong type", `{"path":7}`, "parameter 'path' must be string, got number", nil},
		{"bad integer", `{"path":"x","limit":"lots"}`, "parameter 'limit' must be integer", nil},
		{"fractional integer", `{"path":"x","limit":1.5}`, "parameter 'limit' must be integer", nil},
		{"bad array", `{"path":"x","tags":"a"}`, "parameter 'tags' must be array", nil},
		{"bad object", `{"path":"x","nested":[]}`, "parameter 'nested' must be object", nil},
		{"bad enum", `{"path":"x","mode":"medium"}`, "must be one of", nil},
		{"not object", `[1,2]`, "must be a JSON object, got array", nil},
		{"invalid json", `{"path":`, "not valid JSON", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := r.Execute(context.Background(), "read_file", json.RawMessage(tt.args))
			if tt.wantErr != "" {
				if !res.IsError || !strings.Contains(res.Content, tt.wantErr) {
					t.Fatalf("got %+v, want error containing %q", res, tt.wantErr)
				}
				return
			}
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(res.Content), &got); err != nil {
				t.Fatalf("tool got invalid JSON %q: %v", res.Content, err)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s = %#v, want %#v", k, got[k], v)
				}
			}
		})
	}
}

// Schemas that come from JSON (MCP servers, config) decode "required" as
// []any rather than []string; validation must still enforce it.
func TestExecuteValidatesJSONDecodedSchema(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"q":{"type":"string"},"n":{"type":"integer"}},"required":["q"]}`), &schema); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	if err := r.Register(&fakeTool{name: "mcp_x_search", params: schema}); err != nil {
		t.Fatal(err)
	}
	if res := r.Execute(context.Background(), "mcp_x_search", json.RawMessage(`{"n":1}`)); !res.IsError ||
		!strings.Contains(res.Content, "missing required parameter 'q'") {
		t.Errorf("got %+v", res)
	}
	if res := r.Execute(context.Background(), "mcp_x_search", json.RawMessage(`{"q":"go","n":"2"}`)); res.IsError ||
		res.Content != `{"n":2,"q":"go"}` {
		t.Errorf("got %+v", res)
	}
}

func TestExecuteRecoversPanics(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(&fakeTool{name: "boom", params: Object(nil), run: func(context.Context, json.RawMessage) Result {
		panic("kaboom")
	}})
	res := r.Execute(context.Background(), "boom", nil)
	if !res.IsError || !strings.Contains(res.Content, "panic: kaboom") {
		t.Errorf("got %+v", res)
	}
}

func TestExecuteCancelledContext(t *testing.T) {
	r := newTestRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := r.Execute(ctx, "exec", json.RawMessage(`{"command":"ls"}`))
	if !res.IsError || !strings.Contains(res.Content, "canceled") {
		t.Errorf("got %+v", res)
	}
}

func TestExecuteTruncates(t *testing.T) {
	big := strings.Repeat("é", 50)
	r := NewRegistry(WithMaxResultChars(10))
	_ = r.Register(&fakeTool{name: "big", params: Object(nil), run: func(context.Context, json.RawMessage) Result {
		return Text(big)
	}})
	res := r.Execute(context.Background(), "big", nil)
	if !strings.HasPrefix(res.Content, strings.Repeat("é", 10)+"\n... (truncated, 40 more characters)") {
		t.Errorf("got %q", res.Content)
	}

	unlimited := NewRegistry(WithMaxResultChars(0))
	_ = unlimited.Register(&fakeTool{name: "big", params: Object(nil), run: func(context.Context, json.RawMessage) Result {
		return Text(big)
	}})
	if got := unlimited.Execute(context.Background(), "big", nil).Content; got != big {
		t.Errorf("unlimited registry truncated output")
	}
}

func TestToolErrorGetsHintOnce(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(&fakeTool{name: "fail", params: Object(nil), run: func(context.Context, json.RawMessage) Result {
		return Result{Content: "Error: nope" + errorHint, IsError: true}
	}})
	res := r.Execute(context.Background(), "fail", nil)
	if strings.Count(res.Content, errorHint) != 1 {
		t.Errorf("got %q", res.Content)
	}
}

func TestConcurrentAccess(t *testing.T) {
	r := newTestRegistry(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if i%5 == 0 {
				name := "extra_" + string(rune('a'+i))
				_ = r.Register(&fakeTool{name: name, params: Object(nil)})
				r.Unregister(name)
			}
			_ = r.Definitions()
			_ = r.Execute(context.Background(), "exec", json.RawMessage(`{"command":"x"}`))
		})
	}
	wg.Wait()
}

func TestDecodeArgs(t *testing.T) {
	type args struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	got, err := DecodeArgs[args](json.RawMessage(`{"path":"a","limit":3}`))
	if err != nil || got.Path != "a" || got.Limit != 3 {
		t.Errorf("got %+v, %v", got, err)
	}
	if _, err := DecodeArgs[args](nil); err != nil {
		t.Errorf("empty args: %v", err)
	}
	if _, err := DecodeArgs[args](json.RawMessage(`{"limit":"x"}`)); err == nil {
		t.Error("expected error")
	}
}

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"abc", "", 3}, {"kitten", "sitting", 3}, {"readfile", "redfile", 1},
	}
	for _, tt := range tests {
		if got := levenshtein(tt.a, tt.b); got != tt.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
