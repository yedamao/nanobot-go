package agent

import (
	"context"

	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// Hooks observes a run. OnToolStart and OnToolEnd may be called
// concurrently when read-only tools run in parallel, so implementations
// must be safe for concurrent use. Embed NopHooks to implement only some
// methods.
type Hooks interface {
	// BeforeIteration is called before each model request. messages must
	// not be modified.
	BeforeIteration(ctx context.Context, iteration int, messages []providers.Message)
	// OnToolStart is called before a tool call executes.
	OnToolStart(ctx context.Context, call providers.ToolCall)
	// OnToolEnd is called with the result of a tool call, including calls
	// that were rejected without executing.
	OnToolEnd(ctx context.Context, call providers.ToolCall, result tools.Result)
	// OnContent is called with non-empty assistant text from each response.
	OnContent(ctx context.Context, content string)
}

// NopHooks implements Hooks with no-ops.
type NopHooks struct{}

func (NopHooks) BeforeIteration(context.Context, int, []providers.Message)   {}
func (NopHooks) OnToolStart(context.Context, providers.ToolCall)             {}
func (NopHooks) OnToolEnd(context.Context, providers.ToolCall, tools.Result) {}
func (NopHooks) OnContent(context.Context, string)                           {}
