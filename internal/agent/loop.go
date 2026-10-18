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
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/session"
	"github.com/yedamao/nanobot-go/internal/tools"
)

// Built-in chat commands handled by the loop without calling the model.
const (
	CommandNew  = "/new"
	CommandHelp = "/help"
)

const (
	helpText = "Commands:\n" +
		"  /new   start a new conversation (clears this session's history)\n" +
		"  /help  show this help"
	newSessionText = "New session started."
	errorReplyText = "Sorry, I hit an error while processing that: %v"
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
	MaxHistoryMessages int
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

	mu      sync.Mutex
	workers map[string]*worker
	wg      sync.WaitGroup
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
	content string
	err     error
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
	return &Loop{cfg: cfg, runner: NewRunner(cfg.Provider), log: log, workers: map[string]*worker{}}, nil
}

// Run consumes inbound messages from the bus until ctx ends, then waits for
// in-flight turns to stop (they see the same ctx and are canceled) and
// returns ctx.Err().
func (l *Loop) Run(ctx context.Context) error {
	if l.cfg.Bus == nil {
		return errors.New("agent loop: bus is required")
	}
	defer l.Wait()
	for {
		msg, err := l.cfg.Bus.ConsumeInbound(ctx)
		if err != nil {
			return err
		}
		l.enqueue(job{ctx: ctx, msg: msg})
	}
}

// Wait blocks until every session worker has exited.
func (l *Loop) Wait() { l.wg.Wait() }

// ProcessDirect runs one turn for content in sessionKey and returns the
// reply, bypassing the bus. It is serialized with other turns of the same
// session. Canceling ctx cancels the turn; a failed or canceled turn is not
// saved to the session.
func (l *Loop) ProcessDirect(ctx context.Context, content, sessionKey string) (string, error) {
	channel, chatID, ok := strings.Cut(sessionKey, ":")
	if !ok {
		channel, chatID = "direct", sessionKey
	}
	reply := make(chan result, 1)
	l.enqueue(job{
		ctx:   ctx,
		msg:   bus.InboundMessage{Channel: channel, ChatID: chatID, SenderID: "user", Content: content, SessionKeyOverride: sessionKey},
		reply: reply,
	})
	select {
	case r := <-reply:
		return r.content, r.err
	case <-ctx.Done():
		// The worker still finishes the job (and observes the same ctx);
		// reply is buffered so it never blocks.
		return "", ctx.Err()
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
		l.wg.Add(1)
		go l.work(key, w)
	}
	w.queue = append(w.queue, j)
}

// work drains one session's queue, then removes the worker. The check for
// an empty queue and the removal happen under the same lock as enqueue, so
// no job is ever stranded.
func (l *Loop) work(key string, w *worker) {
	defer l.wg.Done()
	for {
		l.mu.Lock()
		if len(w.queue) == 0 {
			delete(l.workers, key)
			l.mu.Unlock()
			return
		}
		j := w.queue[0]
		w.queue[0] = job{}
		w.queue = w.queue[1:]
		l.mu.Unlock()
		l.handle(j)
	}
}

// handle runs one job and delivers its result.
func (l *Loop) handle(j job) {
	content, err := l.turn(j.ctx, j.msg)
	if j.reply != nil {
		j.reply <- result{content: content, err: err}
		return
	}
	if err != nil {
		if j.ctx.Err() != nil {
			return // shutting down: nothing to deliver
		}
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
// dangling user message or partial tool exchange in history.
func (l *Loop) turn(ctx context.Context, msg bus.InboundMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	key := msg.SessionKey()
	sess, err := l.cfg.Sessions.GetOrCreate(key)
	if err != nil {
		return "", fmt.Errorf("load session: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(msg.Content)) {
	case CommandNew:
		sess.Clear()
		if err := l.cfg.Sessions.Save(sess); err != nil {
			return "", fmt.Errorf("reset session: %w", err)
		}
		return newSessionText, nil
	case CommandHelp:
		return helpText, nil
	}

	messages := l.cfg.Context.BuildMessages(sess.History(l.cfg.MaxHistoryMessages), msg.Content, msg.Channel)
	res, err := l.runner.Run(ctx, RunSpec{
		Messages:      messages,
		Tools:         l.cfg.Tools,
		Model:         l.cfg.Model,
		MaxIterations: l.cfg.MaxIterations,
		MaxTokens:     l.cfg.MaxTokens,
		Temperature:   l.cfg.Temperature,
		Hooks:         l.cfg.Hooks,
	})
	if err != nil {
		return "", err
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
	return res.FinalContent, nil
}
