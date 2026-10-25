package heartbeat

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yedamao/nanobot-go/internal/fsutil"
	"github.com/yedamao/nanobot-go/internal/providers"
)

const (
	// EvaluatorPromptFile is the workspace file that replaces the
	// notification check's system prompt, like nanobot's
	// prompts/evaluator.md.
	EvaluatorPromptFile = "prompts/evaluator.md"
	// maxEvaluatorPromptChars matches nanobot's WORKSPACE_PROMPT_MAX_CHARS.
	maxEvaluatorPromptChars = 32000
	// evaluateToolName is the tool the check answers with.
	evaluateToolName = "evaluate_notification"
	// evaluateMaxTokens matches nanobot's evaluate_response.
	evaluateMaxTokens = 4096
)

// DefaultEvaluatorPrompt is the notification check's system prompt,
// nanobot's templates/agent/evaluator.md.
const DefaultEvaluatorPrompt = `You are a notification gate for a background agent. You will be given the original task and the agent's response. Call the evaluate_notification tool to decide whether the user should be notified.

Notify when the response contains actionable information, errors, completed deliverables, scheduled reminder/timer completions, or anything the user explicitly asked to be reminded about.

A user-scheduled reminder should usually notify even when the response is brief or mostly repeats the original reminder.

Suppress when the response is a routine status check with nothing new, a confirmation that everything is normal, or essentially empty.

Also suppress when the response contains meta-reasoning about the task itself — descriptions of internal instructions, references to configuration files (e.g. HEARTBEAT.md, AWARENESS.md), or decision logic about whether to notify the user. The user should never see the agent reasoning about whether to speak.`

var evaluateTool = providers.ToolDef{
	Name:        evaluateToolName,
	Description: "Decide whether the user should be notified about this background task result.",
	Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"should_notify": map[string]any{
				"type":        "boolean",
				"description": "true = result contains actionable/important info the user should see; false = routine or empty, safe to suppress",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "One-sentence reason for the decision",
			},
		},
		"required": []string{"should_notify"},
	},
}

// EvaluatorPrompt returns the workspace's prompts/evaluator.md, capped,
// or DefaultEvaluatorPrompt when it is missing, unreadable or blank.
func EvaluatorPrompt(workspace string, log *slog.Logger) string {
	data, err := fsutil.ReadRegularFile(filepath.Join(workspace, filepath.FromSlash(EvaluatorPromptFile)), 4*maxEvaluatorPromptChars+4)
	if err != nil || !utf8.Valid(data) {
		return DefaultEvaluatorPrompt
	}
	text := strings.TrimRightFunc(string(data), unicode.IsSpace)
	if strings.TrimSpace(text) == "" {
		return DefaultEvaluatorPrompt
	}
	if n := utf8.RuneCountInString(text); n > maxEvaluatorPromptChars {
		if log != nil {
			log.Warn("heartbeat: workspace evaluator prompt truncated", "chars", n, "max", maxEvaluatorPromptChars)
		}
		text = string([]rune(text)[:maxEvaluatorPromptChars])
	}
	return text
}

// Evaluate asks the model whether response, the result of task, should
// reach the user, like nanobot's evaluate_response with
// default_notify=False: any failure (an error, no tool call, a truncated
// reply, a malformed answer) means no.
func Evaluate(ctx context.Context, p providers.Provider, model, systemPrompt, task, response string, log *slog.Logger) bool {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	temp := 0.0
	resp, err := p.Chat(ctx, providers.ChatRequest{
		Model: model,
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: systemPrompt},
			{Role: providers.RoleUser, Content: "## Original task\n" + task + "\n\n## Agent response\n" + response},
		},
		Tools:       []providers.ToolDef{evaluateTool},
		MaxTokens:   evaluateMaxTokens,
		Temperature: &temp,
	})
	if err != nil {
		log.Warn("heartbeat: evaluation failed; not notifying", "error", err)
		return false
	}
	if len(resp.ToolCalls) == 0 || resp.FinishReason == providers.FinishLength || resp.FinishReason == providers.FinishError {
		log.Warn("heartbeat: evaluation returned no usable tool call; not notifying", "finish_reason", resp.FinishReason)
		return false
	}
	var args struct {
		ShouldNotify any `json:"should_notify"`
		Reason       any `json:"reason"`
	}
	if err := json.Unmarshal(resp.ToolCalls[0].Arguments, &args); err != nil {
		log.Warn("heartbeat: evaluation arguments are not an object; not notifying", "error", err)
		return false
	}
	notify, ok := args.ShouldNotify.(bool)
	if !ok {
		log.Warn("heartbeat: evaluation should_notify is not a boolean; not notifying")
		return false
	}
	log.Info("heartbeat: evaluated", "should_notify", notify, "reason", args.Reason)
	return notify
}
