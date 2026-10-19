package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// DefaultMaxResultChars caps tool output sent back to the model, matching
// nanobot's maxToolResultChars.
const DefaultMaxResultChars = 16000

// errorHint is appended once to every error result to steer the model.
const errorHint = "\n\n[Consider this result in the context of the user's goal and decide whether and how to proceed.]"

// mcpPrefix marks tools contributed by MCP servers; they sort after built-ins.
const mcpPrefix = "mcp_"

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Registry holds the tools available to an agent. It is safe for
// concurrent use.
type Registry struct {
	maxResultChars int

	mu    sync.RWMutex
	tools map[string]Tool
	defs  []Definition // cached; nil when stale
}

// Option configures a Registry.
type Option func(*Registry)

// WithMaxResultChars sets the result truncation cap. n <= 0 disables it.
func WithMaxResultChars(n int) Option {
	return func(r *Registry) { r.maxResultChars = n }
}

// NewRegistry returns an empty registry.
func NewRegistry(opts ...Option) *Registry {
	r := &Registry{maxResultChars: DefaultMaxResultChars, tools: map[string]Tool{}}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Register adds a tool. It fails if the name is invalid or already taken.
func (r *Registry) Register(t Tool) error {
	return r.RegisterEach(t)[0]
}

// RegisterEach adds several tools in one step, so a concurrent
// Definitions call sees all of them or none. Each tool is checked on its
// own: the result holds one error (nil on success) per tool, and a
// rejected tool does not stop the others.
func (r *Registry) RegisterEach(ts ...Tool) []error {
	errs := make([]error, len(ts))
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, t := range ts {
		if t == nil {
			errs[i] = errors.New("register tool: nil tool")
			continue
		}
		name := t.Name()
		if !validName.MatchString(name) {
			errs[i] = fmt.Errorf("register tool %q: name must match %s", name, validName)
			continue
		}
		if _, ok := r.tools[name]; ok {
			errs[i] = fmt.Errorf("register tool %q: already registered", name)
			continue
		}
		r.tools[name] = t
		r.defs = nil
	}
	return errs
}

// Unregister removes the named tools that are present, in one step.
func (r *Registry) Unregister(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		if _, ok := r.tools[name]; ok {
			delete(r.tools, name)
			r.defs = nil
		}
	}
}

// Get returns the named tool.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names returns the registered tool names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Definitions returns tool definitions in a stable order: built-in tools
// sorted by name, then MCP tools sorted by name. A stable order keeps the
// prompt prefix cacheable. The returned slice must not be modified.
func (r *Registry) Definitions() []Definition {
	r.mu.RLock()
	defs := r.defs
	r.mu.RUnlock()
	if defs != nil {
		return defs
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.defs != nil {
		return r.defs
	}
	defs = make([]Definition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, Definition{Name: t.Name(), Description: t.Description(), Parameters: t.Parameters()})
	}
	slices.SortFunc(defs, func(a, b Definition) int {
		am, bm := strings.HasPrefix(a.Name, mcpPrefix), strings.HasPrefix(b.Name, mcpPrefix)
		if am != bm {
			if am {
				return 1
			}
			return -1
		}
		return strings.Compare(a.Name, b.Name)
	})
	r.defs = defs
	return defs
}

// Execute resolves, validates and runs a tool call. It never panics: unknown
// tools, malformed arguments and tool panics all become error results.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) Result {
	res := r.execute(ctx, name, args)
	if res.IsError && !strings.HasSuffix(res.Content, errorHint) {
		res.Content += errorHint
	}
	res.Content = Truncate(res.Content, r.maxResultChars)
	return res
}

func (r *Registry) execute(ctx context.Context, name string, args json.RawMessage) (res Result) {
	t, ok := r.Get(name)
	if !ok {
		hint := ""
		if s := r.suggest(name); s != "" {
			hint = fmt.Sprintf(" Did you mean '%s'? Tool names must match exactly.", s)
		}
		return Errorf("Tool '%s' not found.%s Available: %s", name, hint, strings.Join(r.Names(), ", "))
	}

	params, err := normalizeArgs(args, t.Parameters())
	if err != nil {
		return Errorf("Tool '%s' %v", name, err)
	}
	if errs := validateArgs(params, t.Parameters()); len(errs) > 0 {
		return Errorf("Invalid parameters for tool '%s': %s", name, strings.Join(errs, "; "))
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return Errorf("Tool '%s': encode arguments: %v", name, err)
	}

	defer func() {
		if p := recover(); p != nil {
			res = Errorf("executing %s: panic: %v", name, p)
		}
	}()
	if err := ctx.Err(); err != nil {
		return Errorf("executing %s: %v", name, err)
	}
	return t.Execute(ctx, raw)
}

// suggest returns the registered name closest to name, or "".
func (r *Registry) suggest(name string) string {
	key := lookupKey(name)
	if key == "" {
		return ""
	}
	best, bestDist := "", math.MaxInt
	for _, n := range r.Names() {
		k := lookupKey(n)
		if k == key {
			return n
		}
		d := levenshtein(key, k)
		if d < bestDist {
			best, bestDist = n, d
		}
	}
	if bestDist <= max(2, len(key)/3) {
		return best
	}
	return ""
}

// lookupKey normalizes names for suggestions only, never for execution.
func lookupKey(s string) string {
	var b strings.Builder
	for _, c := range s {
		if unicode.IsLetter(c) || unicode.IsDigit(c) {
			b.WriteRune(unicode.ToLower(c))
		}
	}
	return b.String()
}

func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// normalizeArgs decodes raw arguments into an object. Like nanobot, it
// accepts empty/null arguments, a JSON-encoded string containing the object,
// and a lone {"arguments": ...} wrapper when the schema has no such property.
func normalizeArgs(raw json.RawMessage, schema map[string]any) (map[string]any, error) {
	v, err := decodeLoose(raw)
	if err != nil {
		return nil, err
	}
	if m, ok := v.(map[string]any); ok && len(m) == 1 {
		if inner, ok := m["arguments"]; ok {
			if _, declared := properties(schema)["arguments"]; !declared {
				switch x := inner.(type) {
				case nil:
					v = map[string]any{}
				case string:
					v = decodeString(x)
				default:
					v = x
				}
			}
		}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parameters must be a JSON object, got %s", jsonType(v))
	}
	return m, nil
}

// decodeLoose decodes JSON, unwrapping one level of string encoding.
func decodeLoose(raw json.RawMessage) (any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	var v any
	if err := unmarshalNumber(raw, &v); err != nil {
		return nil, fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	if s, ok := v.(string); ok {
		return decodeString(s), nil
	}
	return v, nil
}

// decodeString parses a string that holds a JSON object or array. Other
// strings are returned unchanged; a blank string means no arguments.
func decodeString(s string) any {
	t := strings.TrimSpace(s)
	if t == "" {
		return map[string]any{}
	}
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		var inner any
		if err := unmarshalNumber([]byte(t), &inner); err == nil {
			return inner
		}
	}
	return s
}

func unmarshalNumber(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

func properties(schema map[string]any) map[string]any {
	p, _ := schema["properties"].(map[string]any)
	return p
}

func requiredNames(schema map[string]any) []string {
	switch req := schema["required"].(type) {
	case []string:
		return req
	case []any:
		out := make([]string, 0, len(req))
		for _, x := range req {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// validateArgs checks required parameters and the basic types of declared
// properties, coercing obvious string-encoded scalars in place (the model
// sometimes sends "5" for an integer).
func validateArgs(params map[string]any, schema map[string]any) []string {
	var errs []string
	for _, name := range requiredNames(schema) {
		if v, ok := params[name]; !ok || v == nil {
			errs = append(errs, fmt.Sprintf("missing required parameter '%s'", name))
		}
	}
	props := properties(schema)
	for _, name := range slices.Sorted(maps.Keys(params)) {
		ps, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		v := params[name]
		if v == nil {
			continue
		}
		typ, _ := ps["type"].(string)
		cv, ok := coerce(v, typ)
		if !ok {
			errs = append(errs, fmt.Sprintf("parameter '%s' must be %s, got %s", name, typ, jsonType(v)))
			continue
		}
		params[name] = cv
		if enum, ok := ps["enum"].([]any); ok && !enumContains(enum, cv) {
			errs = append(errs, fmt.Sprintf("parameter '%s' must be one of %v", name, enum))
		} else if enum, ok := ps["enum"].([]string); ok {
			if s, _ := cv.(string); !slices.Contains(enum, s) {
				errs = append(errs, fmt.Sprintf("parameter '%s' must be one of %v", name, enum))
			}
		}
	}
	return errs
}

func enumContains(enum []any, v any) bool {
	for _, e := range enum {
		if fmt.Sprint(e) == fmt.Sprint(v) {
			return true
		}
	}
	return false
}

// coerce checks v against a JSON Schema type and converts string-encoded
// scalars. Unknown or empty types accept anything.
func coerce(v any, typ string) (any, bool) {
	switch typ {
	case "string":
		_, ok := v.(string)
		return v, ok
	case "integer":
		switch x := v.(type) {
		case json.Number:
			if i, err := x.Int64(); err == nil {
				return json.Number(strconv.FormatInt(i, 10)), true
			}
			if f, err := x.Float64(); err == nil && f == math.Trunc(f) {
				return json.Number(strconv.FormatInt(int64(f), 10)), true
			}
		case string:
			if i, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
				return json.Number(strconv.FormatInt(i, 10)), true
			}
		}
		return v, false
	case "number":
		switch x := v.(type) {
		case json.Number:
			return x, true
		case string:
			if _, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
				return json.Number(strings.TrimSpace(x)), true
			}
		}
		return v, false
	case "boolean":
		switch x := v.(type) {
		case bool:
			return x, true
		case string:
			if b, err := strconv.ParseBool(strings.TrimSpace(x)); err == nil {
				return b, true
			}
		}
		return v, false
	case "array":
		_, ok := v.([]any)
		return v, ok
	case "object":
		_, ok := v.(map[string]any)
		return v, ok
	}
	return v, true
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case json.Number, float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// Truncate shortens s to at most maxChars runes plus a trailing note.
// maxChars <= 0 disables truncation.
func Truncate(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars || utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxChars]) + fmt.Sprintf("\n... (truncated, %d more characters)", len(runes)-maxChars)
}
