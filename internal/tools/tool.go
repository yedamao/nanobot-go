// Package tools defines the Tool interface and the Registry that validates
// and executes tool calls coming from the model. It mirrors
// nanobot/agent/tools/base.py and registry.py.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool is a capability exposed to the model.
type Tool interface {
	// Name is the identifier the model uses to call the tool.
	Name() string
	// Description tells the model what the tool does.
	Description() string
	// Parameters is a JSON Schema object describing the arguments.
	Parameters() map[string]any
	// ReadOnly reports whether the tool has no side effects. Read-only tools
	// may run concurrently.
	ReadOnly() bool
	// Execute runs the tool. args is always a JSON object that has passed
	// basic schema validation. Failures are reported through Result.IsError,
	// not by panicking.
	Execute(ctx context.Context, args json.RawMessage) Result
}

// Result is the outcome of a tool call, sent back to the model.
type Result struct {
	Content string
	IsError bool
}

// Text returns a successful result.
func Text(s string) Result { return Result{Content: s} }

// Errorf returns an error result. The message is prefixed with "Error: ".
func Errorf(format string, args ...any) Result {
	return Result{Content: "Error: " + fmt.Sprintf(format, args...), IsError: true}
}

// Definition is the provider-neutral description of a tool.
type Definition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// DecodeArgs unmarshals tool arguments into T.
func DecodeArgs[T any](args json.RawMessage) (T, error) {
	var v T
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, &v); err != nil {
		return v, fmt.Errorf("invalid arguments: %w", err)
	}
	return v, nil
}

// Object builds a JSON Schema object with the given properties.
func Object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// String builds a string property schema.
func String(desc string) map[string]any { return prop("string", desc) }

// Integer builds an integer property schema.
func Integer(desc string) map[string]any { return prop("integer", desc) }

// Boolean builds a boolean property schema.
func Boolean(desc string) map[string]any { return prop("boolean", desc) }

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}
