// Package agent runs the model/tool loop. Runner ports the core semantics of
// nanobot/agent/runner.py: it calls the provider, executes requested tools,
// feeds results back, and stops on a final answer or the iteration limit.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// DefaultMaxIterations matches nanobot's agents.defaults.maxToolIterations.
const DefaultMaxIterations = 200

const (
	// maxEmptyRetries bounds re-requests after a blank response.
	maxEmptyRetries = 2
	// maxLengthRecoveries bounds continuations after finish_reason=length.
	maxLengthRecoveries = 3
	// lengthRecoveryTailChars is how much already-delivered text the
	// continuation prompt quotes.
	lengthRecoveryTailChars = 64
)

// Stop reasons reported in RunResult.StopReason.
const (
	StopCompleted     = "completed"
	StopMaxIterations = "max_iterations"
	// StopTruncated means output kept hitting the token limit after
	// maxLengthRecoveries continuations.
	StopTruncated = "truncated"
	// StopEmpty means the model kept returning blank responses.
	StopEmpty    = "empty_response"
	StopError    = "error"
	StopCanceled = "canceled"
)

// Prompts and fallback texts, adapted from nanobot/utils/runtime.py.
const (
	lengthRecoveryPrompt = "The previous assistant response was cut off. Continue the same response from its " +
		"exact endpoint. Output only new continuation text in the same language and style. " +
		"Do not acknowledge this instruction, restart the response, repeat its title or any " +
		"existing text, recap, or apologize."
	budgetExhaustedPrompt = "The tool-call budget for this turn is exhausted. Based only on the " +
		"conversation and tool results above, provide a concise final response to " +
		"the user. Do not call or request tools. Do not claim the task is complete " +
		"unless the evidence above clearly shows it is complete. State what was " +
		"done, what remains, and the best next step if anything is incomplete."
	truncatedToolResult = "Error: output truncated; tool not executed. Retry with smaller output " +
		"(for example, split large content across several smaller tool calls)."
	emptyFinalMessage = "I completed the tool steps but couldn't produce a final answer. " +
		"Please try again or narrow the task."
	truncatedFinalMessage = "My response kept exceeding the output limit, so I stopped. " +
		"Please ask for a shorter answer or split the task."
	maxIterationsMessage = "I reached the maximum number of tool call iterations (%d) " +
		"without completing the task. You can try breaking the task into smaller steps."
)

// RunSpec describes one run.
type RunSpec struct {
	// Messages is the conversation so far, including the system prompt and
	// the new user message. It is not modified.
	Messages []providers.Message
	// Tools are offered to the model; nil offers none.
	Tools *tools.Registry
	// Model overrides the provider's default model when set.
	Model string
	// MaxIterations caps model requests in the loop; <= 0 means
	// DefaultMaxIterations.
	MaxIterations int
	// MaxTokens caps each response; zero uses the provider default.
	MaxTokens int
	// Temperature is sent with each request when non-nil.
	Temperature *float64
	// Hooks observes the run; nil means NopHooks.
	Hooks Hooks
	// Stream asks the provider to stream its responses, passing the text
	// to Hooks.OnDelta and any thinking to Hooks.OnThinking as it arrives.
	// Providers that cannot stream answer as usual, without deltas.
	Stream bool
}

// RunResult is the outcome of a run.
type RunResult struct {
	FinalContent string
	// Messages is the full conversation: RunSpec.Messages followed by the
	// messages produced by this run. Every assistant tool call has a
	// matching tool message.
	Messages []providers.Message
	// Iterations counts model requests made by the main loop.
	Iterations int
	StopReason string
	Usage      providers.Usage

	start int
}

// NewMessages returns the messages appended by the run.
func (r *RunResult) NewMessages() []providers.Message { return r.Messages[r.start:] }

// Runner runs the model/tool loop against a provider. It holds no per-run
// state and is safe for concurrent use.
type Runner struct {
	provider providers.Provider
}

// NewRunner returns a Runner using p.
func NewRunner(p providers.Provider) *Runner { return &Runner{provider: p} }

// Run executes the loop until the model gives a final answer, the iteration
// limit is reached, the provider fails, or ctx ends. On provider failure or
// cancellation it returns the partial result together with the error.
func (r *Runner) Run(ctx context.Context, spec RunSpec) (*RunResult, error) {
	run := &run{
		runner:  r,
		spec:    spec,
		hooks:   spec.Hooks,
		maxIter: spec.MaxIterations,
		defs:    toolDefs(spec.Tools),
		res:     &RunResult{Messages: slices.Clone(spec.Messages), start: len(spec.Messages)},
	}
	if run.hooks == nil {
		run.hooks = NopHooks{}
	}
	if run.maxIter <= 0 {
		run.maxIter = DefaultMaxIterations
	}
	return run.loop(ctx)
}

// run holds the state of one Run call.
type run struct {
	runner  *Runner
	spec    RunSpec
	hooks   Hooks
	maxIter int
	defs    []providers.ToolDef
	res     *RunResult
	// streamed is set once a response has passed a delta to the hooks.
	streamed bool
	// thinking is set while thinking streams to the hooks, until
	// OnThinkingEnd; thought once any has.
	thinking, thought bool
}

func (r *run) loop(ctx context.Context) (*RunResult, error) {
	var (
		emptyRetries     int
		lengthRecoveries int
		lengthParts      []string // text delivered across length continuations
	)
	for iter := range r.maxIter {
		if err := ctx.Err(); err != nil {
			return r.fail(ctx, err)
		}
		r.hooks.BeforeIteration(ctx, iter, r.res.Messages)
		resp, err := r.chat(ctx, r.res.Messages, r.defs, providers.ToolChoiceAuto)
		r.res.Iterations = iter + 1
		if err != nil {
			return r.fail(ctx, err)
		}
		if resp.Content != "" {
			r.hooks.OnContent(ctx, resp.Content)
		}
		calls := prepareCalls(resp.ToolCalls, iter)

		if resp.FinishReason == providers.FinishLength {
			// Tool calls in a truncated response may have cut-off arguments:
			// never execute them, but answer each one so the history stays
			// valid and the model learns to retry with smaller output.
			if len(calls) > 0 {
				r.appendAssistant(resp.Content, calls)
				for _, c := range calls {
					res := tools.Result{Content: truncatedToolResult, IsError: true}
					r.hooks.OnToolEnd(ctx, c.call, res)
					r.appendToolResult(c.call, res)
				}
			} else {
				r.appendAssistant(resp.Content, nil)
				lengthParts = append(lengthParts, resp.Content)
			}
			if lengthRecoveries >= maxLengthRecoveries {
				final := strings.TrimSpace(strings.Join(lengthParts, ""))
				if final == "" {
					final = truncatedFinalMessage
				}
				if len(calls) > 0 {
					r.appendAssistant(final, nil)
				}
				return r.finish(final, StopTruncated), nil
			}
			lengthRecoveries++
			if len(calls) == 0 {
				r.res.Messages = append(r.res.Messages, providers.Message{Role: providers.RoleUser, Content: lengthRecoveryMessage(strings.Join(lengthParts, ""))})
			}
			continue
		}

		if len(calls) > 0 {
			r.appendAssistant(resp.Content, calls)
			results := r.executeTools(ctx, calls)
			for i, c := range calls {
				r.appendToolResult(c.call, results[i])
			}
			emptyRetries, lengthRecoveries, lengthParts = 0, 0, nil
			continue
		}

		if strings.TrimSpace(resp.Content) == "" {
			if emptyRetries < maxEmptyRetries {
				emptyRetries++
				continue
			}
			r.appendAssistant(emptyFinalMessage, nil)
			return r.finish(emptyFinalMessage, StopEmpty), nil
		}

		r.appendAssistant(resp.Content, nil)
		final := resp.Content
		if len(lengthParts) > 0 {
			final = strings.TrimSpace(strings.Join(lengthParts, "") + resp.Content)
		}
		return r.finish(final, StopCompleted), nil
	}
	return r.forceFinal(ctx)
}

// forceFinal makes one request that forbids tool calls, asking for a final
// answer after the iteration budget is spent, and falls back to a fixed
// message. The tool definitions stay in the request (with tool_choice
// "none") because Anthropic rejects histories containing tool calls when no
// tools are defined.
func (r *run) forceFinal(ctx context.Context) (*RunResult, error) {
	if err := ctx.Err(); err != nil {
		return r.fail(ctx, err)
	}
	msgs := append(slices.Clip(r.res.Messages), providers.Message{Role: providers.RoleUser, Content: budgetExhaustedPrompt})
	final := fmt.Sprintf(maxIterationsMessage, r.maxIter)
	resp, err := r.chat(ctx, msgs, r.defs, providers.ToolChoiceNone)
	switch {
	case err != nil && ctx.Err() != nil:
		return r.fail(ctx, err)
	case err == nil && len(resp.ToolCalls) == 0 && strings.TrimSpace(resp.Content) != "":
		final = resp.Content
		r.hooks.OnContent(ctx, final)
	}
	r.appendAssistant(final, nil)
	return r.finish(final, StopMaxIterations), nil
}

// chat sends one request and accounts for its usage. A response with
// FinishError is turned into an error. With RunSpec.Stream its text is
// passed to Hooks.OnDelta as it arrives.
func (r *run) chat(ctx context.Context, msgs []providers.Message, defs []providers.ToolDef, choice providers.ToolChoice) (*providers.ChatResponse, error) {
	req := providers.ChatRequest{
		Model:       r.spec.Model,
		Messages:    msgs,
		Tools:       defs,
		ToolChoice:  choice,
		MaxTokens:   r.spec.MaxTokens,
		Temperature: r.spec.Temperature,
	}
	if r.spec.Stream {
		first := true
		req.OnThinking = func(d string) {
			if d == "" {
				return
			}
			if !r.thinking && r.thought {
				d = "\n\n" + d // keep it apart from earlier thinking
			}
			r.thinking, r.thought = true, true
			r.hooks.OnThinking(ctx, d)
		}
		req.OnDelta = func(d string) {
			if d == "" {
				return
			}
			r.endThinking(ctx)
			if first && r.streamed {
				d = "\n\n" + d // keep it apart from the text of an earlier response
			}
			first, r.streamed = false, true
			r.hooks.OnDelta(ctx, d)
		}
	}
	resp, err := r.runner.provider.Chat(ctx, req)
	r.endThinking(ctx)
	if err != nil {
		return nil, err
	}
	r.res.Usage = r.res.Usage.Add(resp.Usage)
	if resp.FinishReason == providers.FinishError {
		return nil, fmt.Errorf("%s: model returned an error: %s", r.runner.provider.Name(), resp.Content)
	}
	return resp, nil
}

// endThinking calls Hooks.OnThinkingEnd if thinking is streaming.
func (r *run) endThinking(ctx context.Context) {
	if r.thinking {
		r.thinking = false
		r.hooks.OnThinkingEnd(ctx)
	}
}

func (r *run) fail(ctx context.Context, err error) (*RunResult, error) {
	reason := StopError
	if ctx.Err() != nil {
		reason = StopCanceled
		if !errors.Is(err, ctx.Err()) {
			err = fmt.Errorf("%w (%w)", ctx.Err(), err)
		}
	}
	r.res.StopReason = reason
	return r.res, fmt.Errorf("agent run: %w", err)
}

func (r *run) finish(final, reason string) *RunResult {
	r.res.FinalContent = final
	r.res.StopReason = reason
	return r.res
}

func (r *run) appendAssistant(content string, calls []preparedCall) {
	if content == "" && len(calls) == 0 {
		return
	}
	m := providers.Message{Role: providers.RoleAssistant, Content: content}
	for _, c := range calls {
		m.ToolCalls = append(m.ToolCalls, c.call)
	}
	r.res.Messages = append(r.res.Messages, m)
}

func (r *run) appendToolResult(call providers.ToolCall, res tools.Result) {
	r.res.Messages = append(r.res.Messages, providers.Message{
		Role:       providers.RoleTool,
		ToolCallID: call.ID,
		Name:       call.Name,
		Content:    res.Content,
		IsError:    res.IsError,
	})
}

// preparedCall is a tool call made safe for history and execution.
type preparedCall struct {
	call providers.ToolCall
	// parseErr is set when the arguments were not valid JSON; the call is
	// answered with an error instead of executing.
	parseErr error
}

// prepareCalls assigns missing IDs and replaces arguments that are not
// valid JSON with a JSON string holding the raw text, so the history always
// marshals.
func prepareCalls(calls []providers.ToolCall, iter int) []preparedCall {
	out := make([]preparedCall, len(calls))
	for i, c := range calls {
		if c.ID == "" {
			c.ID = fmt.Sprintf("call_%d_%d", iter, i)
		}
		var pc preparedCall
		switch {
		case len(c.Arguments) == 0:
			c.Arguments = json.RawMessage("{}")
		case !json.Valid(c.Arguments):
			var v any
			pc.parseErr = json.Unmarshal(c.Arguments, &v)
			b, _ := json.Marshal(string(c.Arguments))
			c.Arguments = b
		}
		pc.call = c
		out[i] = pc
	}
	return out
}

// executeTools runs calls and returns results in call order. Consecutive
// read-only calls run concurrently; every other call runs alone.
func (r *run) executeTools(ctx context.Context, calls []preparedCall) []tools.Result {
	results := make([]tools.Result, len(calls))
	for i := 0; i < len(calls); {
		j := i + 1
		if r.readOnly(calls[i]) {
			for j < len(calls) && r.readOnly(calls[j]) {
				j++
			}
		}
		if j-i == 1 {
			results[i] = r.executeTool(ctx, calls[i])
		} else {
			var wg sync.WaitGroup
			for k := i; k < j; k++ {
				wg.Go(func() { results[k] = r.executeTool(ctx, calls[k]) })
			}
			wg.Wait()
		}
		i = j
	}
	return results
}

func (r *run) readOnly(c preparedCall) bool {
	if c.parseErr != nil || r.spec.Tools == nil {
		return false
	}
	t, ok := r.spec.Tools.Get(c.call.Name)
	return ok && t.ReadOnly()
}

func (r *run) executeTool(ctx context.Context, c preparedCall) tools.Result {
	r.hooks.OnToolStart(ctx, c.call)
	var res tools.Result
	switch {
	case c.parseErr != nil:
		res = tools.Errorf("arguments for tool '%s' are not valid JSON (%v). Retry with a valid JSON object.", c.call.Name, c.parseErr)
	case r.spec.Tools == nil:
		res = tools.Errorf("Tool '%s' not found. No tools are available.", c.call.Name)
	default:
		res = r.spec.Tools.Execute(ctx, c.call.Name, c.call.Arguments)
	}
	if res.Content == "" {
		res.Content = fmt.Sprintf("(%s completed with no output)", c.call.Name)
	}
	r.hooks.OnToolEnd(ctx, c.call, res)
	return res
}

func toolDefs(reg *tools.Registry) []providers.ToolDef {
	if reg == nil {
		return nil
	}
	defs := reg.Definitions()
	out := make([]providers.ToolDef, len(defs))
	for i, d := range defs {
		out[i] = providers.ToolDef{Name: d.Name, Description: d.Description, Parameters: d.Parameters}
	}
	return out
}

// lengthRecoveryMessage asks the model to continue a cut-off answer,
// quoting the tail it already delivered.
func lengthRecoveryMessage(delivered string) string {
	tail := []rune(delivered)
	tail = tail[max(0, len(tail)-lengthRecoveryTailChars):]
	return lengthRecoveryPrompt + "\n\n" +
		"The following tail was already delivered to the user. Treat it as immutable " +
		"context and do not output it again:\n" +
		"<already_delivered_tail>\n" + string(tail) + "\n</already_delivered_tail>\n" +
		"Begin with the text that belongs immediately after this tail."
}
