package mcp

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNormalizeSchema(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, `{"type":"object","properties":{}}`},
		{"not an object", "nope", `{"type":"object","properties":{}}`},
		{"bare object", map[string]any{"type": "object"}, `{"type":"object","properties":{},"required":[]}`},
		{"missing type", map[string]any{"properties": map[string]any{}}, `{"type":"object","properties":{}}`},
		{"raw JSON", json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
			`{"type":"object","properties":{"a":{"type":"string"}},"required":[]}`},
		{"$defs ref inlined", `{
			"type": "object",
			"properties": {"where": {"$ref": "#/$defs/Loc", "description": "target"}},
			"required": ["where"],
			"$defs": {"Loc": {"type": "object", "properties": {"city": {"type": "string"}}, "description": "a place"}}
		}`, `{
			"type": "object",
			"properties": {"where": {"type": "object", "properties": {"city": {"type": "string"}}, "required": [], "description": "target"}},
			"required": ["where"]
		}`},
		{"definitions and pointer refs", `{
			"type": "object",
			"properties": {
				"a": {"$ref": "#/definitions/A"},
				"b": {"$ref": "#/properties/a"}
			},
			"definitions": {"A": {"type": "integer"}}
		}`, `{
			"type": "object",
			"properties": {"a": {"type": "integer"}, "b": {"type": "integer"}},
			"required": []
		}`},
		{"recursive ref kept with defs", `{
			"type": "object",
			"properties": {"node": {"$ref": "#/$defs/Node"}},
			"$defs": {"Node": {"type": "object", "properties": {"next": {"$ref": "#/$defs/Node"}}}}
		}`, `{
			"type": "object",
			"properties": {"node": {"type": "object", "properties": {"next": {"$ref": "#/$defs/Node"}}, "required": []}},
			"required": [],
			"$defs": {"Node": {"type": "object", "properties": {"next": {"type": "object", "properties": {"next": {"$ref": "#/$defs/Node"}}, "required": []}}, "required": []}}
		}`},
		{"unresolved ref kept", `{"type": "object", "properties": {"x": {"$ref": "#/$defs/Missing"}}}`,
			`{"type": "object", "properties": {"x": {"$ref": "#/$defs/Missing"}}, "required": []}`},
		{"nullable anyOf", `{
			"type": "object",
			"properties": {
				"limit": {"anyOf": [{"type": "integer"}, {"type": "null"}], "description": "max"},
				"tag": {"oneOf": [{"type": "null"}, {"type": "string", "enum": ["a", "b"]}]},
				"mixed": {"anyOf": [{"type": "integer"}, {"type": "string"}]}
			}
		}`, `{
			"type": "object",
			"properties": {
				"limit": {"type": "integer", "description": "max", "nullable": true},
				"tag": {"type": "string", "enum": ["a", "b"], "nullable": true},
				"mixed": {"anyOf": [{"type": "integer"}, {"type": "string"}]}
			},
			"required": []
		}`},
		{"type list and nested items", `{
			"type": "object",
			"properties": {
				"name": {"type": ["string", "null"]},
				"list": {"type": "array", "items": {"anyOf": [{"$ref": "#/$defs/Item"}, {"type": "null"}]}}
			},
			"$defs": {"Item": {"type": "object", "properties": {"id": {"type": ["integer", "null"]}}}}
		}`, `{
			"type": "object",
			"properties": {
				"name": {"type": "string", "nullable": true},
				"list": {"type": "array", "items": {"type": "object", "properties": {"id": {"type": "integer", "nullable": true}}, "required": [], "nullable": true}}
			},
			"required": []
		}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			var before string
			if s, ok := in.(string); ok && strings.HasPrefix(strings.TrimSpace(s), "{") {
				in = mustJSON(t, s)
				data, _ := json.Marshal(in)
				before = string(data)
			}
			got := normalizeSchema(in, nil)
			if want := mustJSON(t, tt.want); !reflect.DeepEqual(got, want) {
				g, _ := json.MarshalIndent(got, "", "  ")
				w, _ := json.MarshalIndent(want, "", "  ")
				t.Errorf("got\n%s\nwant\n%s", g, w)
			}
			if before != "" {
				if data, _ := json.Marshal(in); string(data) != before {
					t.Error("normalizeSchema modified its input")
				}
			}
		})
	}
}

func TestToolName(t *testing.T) {
	valid := regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	tests := []struct{ server, tool, want string }{
		{"github", "search_repos", "mcp_github_search_repos"},
		{"fs", "read.file", "mcp_fs_read_file"},
		{"fs", "a  b//c", "mcp_fs_a_b_c"},
		{"fs", "über-tool", "mcp_fs_ber-tool"},
		{"fs", "x__y", "mcp_fs_x_y"},
	}
	for _, tt := range tests {
		if got := toolName(tt.server, tt.tool); got != tt.want {
			t.Errorf("toolName(%q, %q) = %q, want %q", tt.server, tt.tool, got, tt.want)
		}
	}

	long1 := strings.Repeat("very_long_tool_name_", 4) + "alpha"
	long2 := strings.Repeat("very_long_tool_name_", 4) + "beta"
	n1, n2 := toolName("server", long1), toolName("server", long2)
	if n1 == n2 {
		t.Errorf("long names collide: %s", n1)
	}
	for _, n := range []string{n1, n2} {
		if len(n) > 64 || !valid.MatchString(n) {
			t.Errorf("invalid long name %q (%d chars)", n, len(n))
		}
		if !strings.HasPrefix(n, "mcp_server_very_long_tool_name_") {
			t.Errorf("long name lost its prefix: %s", n)
		}
	}
	if toolName("server", long1) != n1 {
		t.Error("toolName is not deterministic")
	}
	// Exactly 64 characters stays as is.
	exact := strings.Repeat("x", 64-len("mcp_s_"))
	if got := toolName("s", exact); got != "mcp_s_"+exact {
		t.Errorf("64-char name changed: %s", got)
	}
}

func TestRender(t *testing.T) {
	tests := []struct {
		name string
		res  *sdk.CallToolResult
		want string
		err  bool
	}{
		{"text", &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "a"}, &sdk.TextContent{Text: "b"}}}, "a\nb", false},
		{"empty", &sdk.CallToolResult{}, "(no output)", false},
		{"error", &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "bad input"}}, IsError: true}, "bad input", true},
		{"image", &sdk.CallToolResult{Content: []sdk.Content{&sdk.ImageContent{Data: []byte("1234"), MIMEType: "image/png"}}}, "[image: image/png, 4 bytes]", false},
		{"audio", &sdk.CallToolResult{Content: []sdk.Content{&sdk.AudioContent{Data: []byte("12")}}}, "[audio: unknown type, 2 bytes]", false},
		{"link", &sdk.CallToolResult{Content: []sdk.Content{&sdk.ResourceLink{Name: "readme", URI: "file:///r.md"}}}, "[resource link: readme file:///r.md]", false},
		{"text resource", &sdk.CallToolResult{Content: []sdk.Content{&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///a", Text: "contents"}}}}, "contents", false},
		{"blob resource", &sdk.CallToolResult{Content: []sdk.Content{&sdk.EmbeddedResource{Resource: &sdk.ResourceContents{URI: "file:///b", MIMEType: "application/zip", Blob: []byte("xyz")}}}}, "[resource: file:///b, application/zip, 3 bytes]", false},
		{"structured only", &sdk.CallToolResult{StructuredContent: map[string]any{"n": 1}}, `{"n":1}`, false},
	}
	for _, tt := range tests {
		got := render(tt.res)
		if got.Content != tt.want || got.IsError != tt.err {
			t.Errorf("%s: render = %+v, want %q (error %t)", tt.name, got, tt.want, tt.err)
		}
	}
}
