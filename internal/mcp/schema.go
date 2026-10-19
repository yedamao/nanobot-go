package mcp

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
)

// normalizeSchema turns an MCP tool's input schema into one model providers
// accept, porting nanobot's _normalize_schema_for_openai:
//
//   - the root is an object with a properties map;
//   - local $refs ("#/$defs/X", "#/definitions/X", any "#/..." pointer)
//     are inlined, and $defs/definitions dropped once nothing refers to
//     them; recursive or unresolvable refs are left in place along with
//     their definitions;
//   - nullable forms (type ["X","null"], anyOf/oneOf [X, {type: null}])
//     become plain X with "nullable": true;
//   - object schemas get properties and required.
//
// The input is never modified.
func normalizeSchema(raw any, logger *slog.Logger) map[string]any {
	root, ok := toMap(raw)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	r := &refResolver{root: root, logger: logger}
	out, _ := r.resolve(root, nil).(map[string]any)
	if !r.keepDefs {
		delete(out, "$defs")
		delete(out, "definitions")
	}
	out = normalizeNullable(out)
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	if _, ok := out["properties"].(map[string]any); !ok && out["type"] == "object" {
		out["properties"] = map[string]any{}
	}
	return out
}

// toMap returns v as a JSON object. Schemas from the SDK are already
// map[string]any; anything else is round-tripped through JSON.
func toMap(v any) (map[string]any, bool) {
	switch x := v.(type) {
	case nil:
		return nil, false
	case map[string]any:
		return x, true
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

type refResolver struct {
	root     map[string]any
	logger   *slog.Logger
	keepDefs bool // a $ref was left in place
}

// resolve returns a deep copy of v with local $refs inlined. stack holds
// the refs being expanded, to stop on recursion.
func (r *refResolver) resolve(v any, stack []string) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = r.resolve(item, stack)
		}
		return out
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok {
			if inlined, ok := r.inline(x, ref, stack); ok {
				return inlined
			}
		}
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = r.resolve(item, stack)
		}
		return out
	}
	return v
}

// inline replaces a {"$ref": ...} node with its target. Sibling keys such
// as "description" override the target's.
func (r *refResolver) inline(node map[string]any, ref string, stack []string) (any, bool) {
	for _, s := range stack {
		if s == ref {
			r.keepDefs = true // recursive: keep the $ref
			return nil, false
		}
	}
	target, err := lookupPointer(r.root, ref)
	if err != nil {
		r.keepDefs = true
		if r.logger != nil {
			r.logger.Warn("MCP tool schema has an unresolved $ref", "ref", ref, "error", err)
		}
		return nil, false
	}
	resolved := r.resolve(target, append(stack, ref))
	m, ok := resolved.(map[string]any)
	if !ok {
		return resolved, true
	}
	for k, item := range node {
		if k != "$ref" {
			m[k] = r.resolve(item, stack)
		}
	}
	return m, true
}

// lookupPointer resolves a local JSON Pointer ("#/a/b") against root.
func lookupPointer(root map[string]any, ref string) (any, error) {
	if !strings.HasPrefix(ref, "#") {
		return nil, errNotLocal
	}
	pointer, err := url.PathUnescape(ref[1:])
	if err != nil {
		return nil, err
	}
	if pointer == "" {
		return root, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, errNotLocal
	}
	var cur any = root
	for _, part := range strings.Split(pointer[1:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			next, ok := c[part]
			if !ok {
				return nil, errMissingRef
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(c) {
				return nil, errMissingRef
			}
			cur = c[i]
		default:
			return nil, errMissingRef
		}
	}
	return cur, nil
}

type schemaError string

func (e schemaError) Error() string { return string(e) }

const (
	errNotLocal   schemaError = "not a local JSON pointer"
	errMissingRef schemaError = "target not found"
)

// normalizeNullable flattens nullable unions in s and its subschemas. s is
// a fresh copy owned by the caller, so it is updated in place.
func normalizeNullable(s map[string]any) map[string]any {
	if types, ok := s["type"].([]any); ok {
		var nonNull []any
		sawNull := false
		for _, t := range types {
			if t == "null" {
				sawNull = true
			} else {
				nonNull = append(nonNull, t)
			}
		}
		if sawNull && len(nonNull) == 1 {
			s["type"] = nonNull[0]
			s["nullable"] = true
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if branch, ok := nullableBranch(s[key]); ok {
			delete(s, key)
			for k, v := range branch {
				s[k] = v
			}
			s["nullable"] = true
			break
		}
	}

	if props, ok := s["properties"].(map[string]any); ok {
		for name, p := range props {
			if pm, ok := p.(map[string]any); ok {
				props[name] = normalizeNullable(pm)
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if m, ok := s[key].(map[string]any); ok {
			s[key] = normalizeNullable(m)
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if list, ok := s[key].([]any); ok {
			for i, item := range list {
				if m, ok := item.(map[string]any); ok {
					list[i] = normalizeNullable(m)
				}
			}
		}
	}
	for _, key := range []string{"$defs", "definitions"} {
		if defs, ok := s[key].(map[string]any); ok {
			for name, d := range defs {
				if dm, ok := d.(map[string]any); ok {
					defs[name] = normalizeNullable(dm)
				}
			}
		}
	}
	if s["type"] == "object" {
		if _, ok := s["properties"].(map[string]any); !ok {
			s["properties"] = map[string]any{}
		}
		if _, ok := s["required"]; !ok {
			s["required"] = []any{}
		}
	}
	return s
}

// nullableBranch returns the one non-null option of a [X, {type: null}]
// union.
func nullableBranch(v any) (map[string]any, bool) {
	options, ok := v.([]any)
	if !ok {
		return nil, false
	}
	var nonNull []map[string]any
	sawNull := false
	for _, o := range options {
		m, ok := o.(map[string]any)
		if !ok {
			return nil, false
		}
		if m["type"] == "null" {
			sawNull = true
			continue
		}
		nonNull = append(nonNull, m)
	}
	if sawNull && len(nonNull) == 1 {
		return nonNull[0], true
	}
	return nil, false
}
