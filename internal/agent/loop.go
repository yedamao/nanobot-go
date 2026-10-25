package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/memory"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/session"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// Built-in chat commands handled by the loop without calling the model.
const (
	CommandNew     = "/new"
	CommandCompact = "/compact"
	CommandHelp    = "/help"
	// CommandStop cancels the session's turn in flight. Serve handles it
	// as soon as it arrives rather than after the turn it stops.
	CommandStop = "/stop"
)

const (
	helpText = "Commands:\n" +
		"  /new      start a new conversation (clears this session's history)\n" +
		"  /compact  summarize this conversation so far into memory and continue from the summary\n" +
		"  /stop     stop the reply in progress\n" +
		"  /help     show this help"
	stoppedText            = "Stopped."
	nothingToStopText      = "Nothing to stop."
	newSessionText         = "New session started."
	compactDisabledText    = "History consolidation is disabled."
	compactNothingText     = "Nothing to compact."
	compactDoneText        = "Compacted %d messages into a summary."
	errorReplyText         = "Sorry, I hit an error while processing that: %v"
	consolidationFailedLog = "history consolidation failed; replaying the most recent messages only this turn"
)

// LoopConfig wires a Loop.
type LoopConfig struct {
	// Bus is required by Run; ProcessDirect works without it.
	Bus      *bus.MessageBus
	Provider providers.Provider
	Tools    *tools.Registry
	Sessions *session.Manager
	Context  *ContextBuilder
	// Model overrides the provider's default model when set.
	Model         string
	MaxIterations int
	MaxTokens     int
	Temperature   *float64
	// MaxHistoryMessages caps replayed history; <= 0 replays everything.
	// With a Consolidator, the oldest messages are summarized before the
	// cap is reached, so the cap only drops messages when consolidation
	// fails.
	MaxHistoryMessages int
	// Consolidator summarizes old history when the next prompt would be
	// too large, and serves /compact. Nil disables both. Its MaxMessages
	// should match MaxHistoryMessages.
	Consolidator *memory.Consolidator
	// Hooks observes every run; nil means none.
	Hooks Hooks
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Loop is the channel-facing side of the agent (nanobot's AgentLoop). It
// turns inbound messages into runs: it loads the session, builds the
// context, runs the Runner, saves the session and delivers the reply.
//
// Each session key gets its own worker goroutine, so turns within a session
// run in arrival order while different sessions run in parallel. Workers
// exit when their queue drains.
type Loop struct {
	cfg    LoopConfig
	runner *Runner
	log    *slog.Logger

	// mu guards workers and cancels; idle is signaled when the last worker
	// exits. A counter under mu (rather than a WaitGroup) lets Wait run
	// concurrently with enqueues for new sessions.
	mu      sync.Mutex
	idle    *sync.Cond
	workers map[string]*worker
	// cancels holds the cancel func of each session's turn in flight.
	cancels map[string]context.CancelFunc
}

type worker struct {
	queue []job
}

// job is one inbound message. ctx bounds the turn; when reply is set the
// result goes there instead of the bus.
type job struct {
	ctx   context.Context
	msg   bus.InboundMessage
	reply chan<- result
}

type result struct {
	reply Reply
	err   error
}

// Reply is the outcome of one turn.
type Reply struct {
	// Content is the final assistant text.
	Content string
	// Usage sums the tokens of the turn's model requests. It is zero for
	// the built-in commands.
	Usage providers.Usage
}

// NewLoop returns a Loop.
func NewLoop(cfg LoopConfig) (*Loop, error) {
	switch {
	case cfg.Provider == nil:
		return nil, errors.New("agent loop: provider is required")
	case cfg.Sessions == nil:
		return nil, errors.New("agent loop: session manager is required")
	case cfg.Context == nil:
		return nil, errors.New("agent loop: context builder is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	l := &Loop{cfg: cfg, runner: NewRunner(cfg.Provider), log: log, workers: map[string]*worker{}, cancels: map[string]context.CancelFunc{}}
	l.idle = sync.NewCond(&l.mu)
	return l, nil
}

// Run consumes inbound messages from the bus until ctx ends, then waits for
// in-flight turns to stop (they see the same ctx and are canceled) and
// returns ctx.Err().
func (l *Loop) Run(ctx context.Context) error {
	defer l.Wait()
	return l.Serve(ctx, ctx)
}

// Serve consumes inbound messages from the bus until ctx ends and returns
// ctx.Err(). Each turn runs under turnCtx instead, so a caller can stop
// accepting messages while the turns in flight finish: cancel ctx, Wait
// (with whatever deadline it likes), then cancel turnCtx. Serve does not
// wait for in-flight turns.
//
// A /stop message cancels its session's turn in flight right away, whose
// reply is then "Stopped."; with no turn in flight it is queued like any
// message and answered "Nothing to stop.".
func (l *Loop) Serve(ctx, turnCtx context.Context) error {
	if l.cfg.Bus == nil {
		return errors.New("agent loop: bus is required")
	}
	for {
		msg, err := l.cfg.Bus.ConsumeInbound(ctx)
		if err != nil {
			return err
		}
		if isCommand(msg.Content, CommandStop) && l.Cancel(msg.SessionKey()) {
			continue
		}
		l.enqueue(job{ctx: turnCtx, msg: msg})
	}
}

// Cancel cancels the turn in flight in sessionKey, if any, and reports
// whether there was one. Turns queued behind it still run. A canceled
// turn is not saved to the session; taken from the bus, it is answered
// "Stopped.", and a Process caller gets context.Canceled.
func (l *Loop) Cancel(sessionKey string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cancel, ok := l.cancels[sessionKey]
	if ok {
		cancel()
	}
	return ok
}

func isCommand(content, command string) bool {
	return strings.ToLower(strings.TrimSpace(content)) == command
}

// Wait blocks until no session worker is running. Turns enqueued while it
// waits are waited for too.
func (l *Loop) Wait() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.workers) > 0 {
		l.idle.Wait()
	}
}

// ProcessDirect runs one turn for content in sessionKey and returns the
// reply, bypassing the bus. It is serialized with other turns of the same
// session. Canceling ctx cancels the turn; a failed or canceled turn is not
// saved to the session.
func (l *Loop) ProcessDirect(ctx context.Context, content, sessionKey string) (string, error) {
	channel, chatID, ok := strings.Cut(sessionKey, ":")
	if !ok {
		channel, chatID = "direct", sessionKey
	}
	return l.ProcessMessage(ctx, bus.InboundMessage{Channel: channel, ChatID: chatID, SenderID: "user", Content: content, SessionKeyOverride: sessionKey})
}

// ProcessMessage runs one turn for msg in its session and returns the
// reply instead of publishing it, like ProcessDirect but with the
// caller's routing (channel, chat, metadata). Scheduled jobs use it to
// run a turn in the chat they were created from and deliver the reply
// themselves.
func (l *Loop) ProcessMessage(ctx context.Context, msg bus.InboundMessage) (string, error) {
	r, err := l.Process(ctx, msg)
	return r.Content, err
}

// Process is ProcessMessage that also returns the turn's token usage.
func (l *Loop) Process(ctx context.Context, msg bus.InboundMessage) (Reply, error) {
	reply := make(chan result, 1)
	l.enqueue(job{ctx: ctx, msg: msg, reply: reply})
	select {
	case r := <-reply:
		return r.reply, r.err
	case <-ctx.Done():
		// The worker still finishes the job (and observes the same ctx);
		// reply is buffered so it never blocks.
		return Reply{}, ctx.Err()
	}
}

func (l *Loop) enqueue(j job) {
	key := j.msg.SessionKey()
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.workers[key]
	if !ok {
		w = &worker{}
		l.workers[key] = w
		go l.work(key, w)
	}
	w.queue = append(w.queue, j)
}

// work drains one session's queue, then removes the worker. The check for
// an empty queue and the removal happen under the same lock as enqueue, so
// no job is ever stranded.
func (l *Loop) work(key string, w *worker) {
	for {
		l.mu.Lock()
		delete(l.cancels, key)
		if len(w.queue) == 0 {
			delete(l.workers, key)
			if len(l.workers) == 0 {
				l.idle.Broadcast()
			}
			l.mu.Unlock()
			return
		}
		j := w.queue[0]
		w.queue[0] = job{}
		w.queue = w.queue[1:]
		ctx, cancel := context.WithCancel(j.ctx)
		l.cancels[key] = cancel
		l.mu.Unlock()
		l.handle(ctx, j)
		cancel()
	}
}

// handle runs one job under ctx, which Cancel may cancel before j.ctx
// ends, and delivers its result.
func (l *Loop) handle(ctx context.Context, j job) {
	// Progress and streamed deltas follow the reply: bus-driven turns
	// report to their chat, while Process callers deliver (or not)
	// themselves and get no stream.
	var progress Hooks
	if j.reply == nil {
		progress = newProgressHooks(l.cfg.Bus, j.msg)
	}
	r, err := l.turn(ctx, j.msg, progress)
	if j.reply != nil {
		j.reply <- result{reply: r, err: err}
		return
	}
	content := r.Content
	switch {
	case err == nil:
	case j.ctx.Err() != nil:
		return // shutting down: nothing to deliver
	case ctx.Err() != nil:
		content = stoppedText
	default:
		l.log.Error("agent turn failed", "session", j.msg.SessionKey(), "error", err)
		content = fmt.Sprintf(errorReplyText, err)
	}
	out := bus.OutboundMessage{Channel: j.msg.Channel, ChatID: j.msg.ChatID, Content: content, Metadata: maps.Clone(j.msg.Metadata)}
	if err := l.cfg.Bus.PublishOutbound(j.ctx, out); err != nil {
		l.log.Warn("drop outbound message", "session", j.msg.SessionKey(), "error", err)
	}
}

// turn processes one message and returns the reply. The session is only
// updated after a successful run, so a failed provider call never leaves a
// dangling user message or partial tool exchange in history. progress, if
// not nil, observes the run alongside LoopConfig.Hooks, and the run is
// streamed for it. Deltas are never saved: history gets the run's
// messages as for an unstreamed run.
func (l *Loop) turn(ctx context.Context, msg bus.InboundMessage, progress Hooks) (Reply, error) {
	if err := ctx.Err(); err != nil {
		return Reply{}, err
	}
	key := msg.SessionKey()
	sess, err := l.cfg.Sessions.GetOrCreate(key)
	if err != nil {
		return Reply{}, fmt.Errorf("load session: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(msg.Content)) {
	case CommandNew:
		sess.Clear()
		if err := l.cfg.Sessions.Save(sess); err != nil {
			return Reply{}, fmt.Errorf("reset session: %w", err)
		}
		return Reply{Content: newSessionText}, nil
	case CommandCompact:
		content, err := l.compact(ctx, sess)
		return Reply{Content: content}, err
	case CommandHelp:
		return Reply{Content: helpText}, nil
	case CommandStop:
		// Serve cancels a turn in flight without queuing /stop, so here
		// there is none.
		return Reply{Content: nothingToStopText}, nil
	}

	l.consolidate(ctx, sess, msg)
	ctx = tools.WithTurn(ctx, tools.Turn{Channel: msg.Channel, ChatID: msg.ChatID, SessionKey: key, Metadata: msg.Metadata})
	messages := l.cfg.Context.BuildMessages(sess.Summary, sess.History(l.cfg.MaxHistoryMessages), msg.Content, msg.Channel)
	res, err := l.runner.Run(ctx, RunSpec{
		Messages:      messages,
		Tools:         l.cfg.Tools,
		Model:         l.cfg.Model,
		MaxIterations: l.cfg.MaxIterations,
		MaxTokens:     l.cfg.MaxTokens,
		Temperature:   l.cfg.Temperature,
		Hooks:         joinHooks(l.cfg.Hooks, progress),
		Stream:        progress != nil,
	})
	if err != nil {
		return Reply{}, err
	}

	// messages ends with the user message; persist it and everything the
	// run added, but not the system prompt or the replayed history.
	sess.Append(messages[len(messages)-1])
	sess.Append(res.NewMessages()...)
	if err := l.cfg.Sessions.Save(sess); err != nil {
		// The reply is still valid; the next turn retries the save.
		l.log.Error("save session", "session", key, "error", err)
	}
	l.log.Debug("agent turn done", "session", key, "iterations", res.Iterations, "stop", res.StopReason,
		"input_tokens", res.Usage.InputTokens, "output_tokens", res.Usage.OutputTokens)
	return Reply{Content: res.FinalContent, Usage: res.Usage}, nil
}

// consolidate summarizes the oldest unconsolidated messages of sess when
// the prompt for msg would exceed the history limits. A failure is logged
// and the turn goes on with the plain History window, as before
// consolidation existed; nothing is lost and the next turn tries again.
func (l *Loop) consolidate(ctx context.Context, sess *session.Session, msg bus.InboundMessage) {
	c := l.cfg.Consolidator
	if c == nil {
		return
	}
	overhead := memory.EstimateMessages([]providers.Message{
		{Role: providers.RoleSystem, Content: l.cfg.Context.SystemPrompt(msg.Channel)},
		{Role: providers.RoleUser, Content: msg.Content},
	})
	if l.cfg.Tools != nil {
		overhead += memory.EstimateJSON(l.cfg.Tools.Definitions())
	}
	boundary, ok := c.Plan(sess, overhead)
	if !ok {
		return
	}
	if _, err := c.Consolidate(ctx, sess, boundary); err != nil {
		if ctx.Err() == nil {
			l.log.Warn(consolidationFailedLog, "session", sess.Key, "error", err)
		}
		return
	}
	l.saveConsolidated(sess)
}

// compact handles /compact: everything so far is summarized, and later
// turns replay only the summary and what follows.
func (l *Loop) compact(ctx context.Context, sess *session.Session) (string, error) {
	if l.cfg.Consolidator == nil {
		return compactDisabledText, nil
	}
	res, err := l.cfg.Consolidator.Consolidate(ctx, sess, len(sess.Messages))
	if err != nil {
		return "", fmt.Errorf("compact: %w", err)
	}
	if res.Archived == 0 {
		return compactNothingText, nil
	}
	l.saveConsolidated(sess)
	return fmt.Sprintf(compactDoneText, res.Archived), nil
}

// saveConsolidated persists a moved marker right away so the session file
// agrees with history.jsonl even if the turn then fails. A failed save is
// retried by the next successful turn's save; sess stays cached with the
// new marker meanwhile.
func (l *Loop) saveConsolidated(sess *session.Session) {
	if err := l.cfg.Sessions.Save(sess); err != nil {
		l.log.Error("save session after consolidation", "session", sess.Key, "error", err)
	}
}
