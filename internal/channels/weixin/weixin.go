// Package weixin is the personal WeChat channel (nanobot/channels/
// weixin). It talks to Tencent's iLink bot API
// (https://ilinkai.weixin.qq.com) over net/http: getupdates long-polls
// for messages with an opaque cursor, sendmessage delivers replies as
// text in chunks of at most 1800 characters. The bot token comes from a
// QR-code login (`nanobot channels login weixin`, see Login), saved in
// account.json, or from the config.
//
// Every reply needs the context_token of the user's latest message. The
// channel caches it per user, refreshes it through getconfig when it is
// older than a minute, and holds back replies it cannot send (no token
// yet, or the token's send quota is used up) until the user writes
// again, like nanobot.
//
// Senders are checked against allowFrom and the pairing store; an unknown
// sender in a private chat gets a pairing code.
package weixin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/pairing"
)

// Runtime constants, from nanobot's runtime.py (and the reference
// plugin's monitor.ts).
const (
	// retryDelay is the wait after a failed getupdates; after
	// maxConsecutiveFailures in a row the wait is backoffDelay.
	retryDelay             = 2 * time.Second
	backoffDelay           = 30 * time.Second
	maxConsecutiveFailures = 3
	// minPollTimeout bounds a server-suggested long-poll timeout.
	minPollTimeout = 5 * time.Second
	// contextTokenMaxAge is how old a cached context_token may get before
	// a send refreshes it: iLink expires them after ~90-160s of inactivity.
	contextTokenMaxAge = 60 * time.Second
	// contextMessageBudget is how many messages one context_token may
	// send before the channel waits for the user to write again (nanobot:
	// contextMessageBudget, default 8); iLink's own quota is undocumented.
	contextMessageBudget = 8
	// maxDeferredPerChat caps the replies held back for one user.
	maxDeferredPerChat = 3
	// seenMax is how many inbound message IDs are remembered for
	// deduplication.
	seenMax = 1000
	// typingInterval refreshes "typing…"; typingMax stops the indicator
	// of a turn whose reply never comes.
	typingInterval = 5 * time.Second
	typingMax      = 10 * time.Minute
	// typingTicketTTL bounds how long a typing ticket is cached (a random
	// fraction of it, like nanobot); failures back off from
	// ticketRetryMin up to ticketRetryMax.
	typingTicketTTL = 24 * time.Hour
	ticketRetryMin  = 2 * time.Second
	ticketRetryMax  = time.Hour
	// partialTTL is how long a partly sent reply is remembered so that a
	// retry by the channel manager skips the chunks already delivered.
	partialTTL  = 2 * time.Minute
	maxPartials = 256
)

// errLoginExpired is returned by Send once iLink has rejected the token.
var errLoginExpired = errors.New("weixin: the login expired; run 'nanobot channels login weixin --force'")

// Options are the channel's dependencies besides its config section.
type Options struct {
	// Getenv reads the token override; nil ignores the environment.
	Getenv func(string) string
	// Pairing approves senders by pairing code; nil disables pairing, so
	// only allowFrom grants access.
	Pairing *pairing.Store
	// HTTPClient calls iLink; nil builds one that ignores the proxy
	// environment, like nanobot.
	HTTPClient *http.Client
	// DefaultStateDir holds account.json when the config sets no stateDir
	// (see DefaultStateDir).
	DefaultStateDir string
	// Version goes into base_info.bot_agent.
	Version string
}

// Factory returns the channels.Factory for WeChat.
func Factory(opts Options) channels.Factory {
	return func(raw json.RawMessage, deps channels.Deps) (channels.Channel, error) {
		cfg, err := ParseConfig(raw, opts.Getenv)
		if err != nil {
			return nil, err
		}
		return New(cfg, deps, opts)
	}
}

// Channel is a WeChat bot connection.
type Channel struct {
	cfg     Config
	api     *client
	state   stateFile
	bus     *bus.MessageBus
	log     *slog.Logger
	pairing *pairing.Store
	// allow holds the allowFrom entries.
	allow    map[string]bool
	allowAll bool

	// sleep waits between attempts (replaced in tests); it returns
	// ctx.Err() if ctx ends first.
	sleep          func(ctx context.Context, d time.Duration) error
	now            func() time.Time
	retryDelay     time.Duration
	backoffDelay   time.Duration
	typingInterval time.Duration

	// saveMu serializes saveState, so snapshots are written in order.
	saveMu sync.Mutex

	mu      sync.Mutex
	started bool
	stopped bool
	runCtx  context.Context
	cancel  context.CancelFunc
	// wg tracks the poller and the typing indicators.
	wg sync.WaitGroup
	// acct is the token in use, its API host and the getupdates cursor.
	acct account
	// authRequired is set once iLink rejects the token as stale.
	authRequired bool
	// pollTimeout is the long-poll timeout, as suggested by the server.
	pollTimeout time.Duration
	// contexts caches each user's latest context_token.
	contexts map[string]contextEntry
	// sendCounts counts the messages sent per context_token.
	sendCounts map[string]int
	seen       map[string]bool
	seenOrder  []string
	tickets    map[string]*ticketEntry
	typing     map[string]*typingTask
	// deferred holds replies waiting for a fresh context_token, by user.
	deferred map[string][]*delivery
	// partial holds partly sent replies by delivery key.
	partial map[string]*delivery
}

type contextEntry struct {
	token string
	at    time.Time // zero for tokens restored from account.json
}

type ticketEntry struct {
	ticket    string
	nextFetch time.Time
	retry     time.Duration
}

type typingTask struct{ cancel context.CancelFunc }

// delivery is one outbound reply split into chunks. Each chunk has a
// client_id derived from the delivery's id, so a chunk sent again keeps
// its client_id.
type delivery struct {
	id     string
	key    string // chat and content hash, for resuming a retry
	chat   string
	chunks []string
	done   int // chunks delivered
	at     time.Time
}

func (d *delivery) clientID(i int) string {
	sum := sha256.Sum256([]byte(d.id + ":text:" + strconv.Itoa(i)))
	return "nanobot-" + hex.EncodeToString(sum[:])[:20]
}

// New returns a channel for a parsed config (see ParseConfig).
func New(cfg Config, deps channels.Deps, opts Options) (*Channel, error) {
	if deps.Bus == nil {
		return nil, errors.New("weixin: bus is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.StateDir == "" && opts.DefaultStateDir == "" {
		return nil, errors.New("weixin: no state directory")
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Channel{
		cfg:            cfg,
		api:            newClient(opts.HTTPClient, string(cfg.RouteTag), opts.Version),
		state:          stateFile{cfg.StatePath(opts.DefaultStateDir)},
		bus:            deps.Bus,
		log:            log,
		pairing:        opts.Pairing,
		allow:          map[string]bool{},
		sleep:          sleepCtx,
		now:            time.Now,
		retryDelay:     retryDelay,
		backoffDelay:   backoffDelay,
		typingInterval: typingInterval,
		pollTimeout:    cfg.pollWait(),
		contexts:       map[string]contextEntry{},
		sendCounts:     map[string]int{},
		seen:           map[string]bool{},
		tickets:        map[string]*ticketEntry{},
		typing:         map[string]*typingTask{},
		deferred:       map[string][]*delivery{},
		partial:        map[string]*delivery{},
	}
	for _, a := range cfg.AllowFrom {
		a = strings.TrimSpace(a)
		if a == "*" {
			c.allowAll = true
		}
		c.allow[a] = true
	}
	return c, nil
}

// Name implements channels.Channel.
func (c *Channel) Name() string { return Name }

// Start picks the bot token (see chooseAccount) and starts long polling.
// Without a token it fails: unlike nanobot, the gateway never shows a QR
// code; log in with `nanobot channels login weixin` first.
func (c *Channel) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return errors.New("weixin: already started")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("weixin: start: %w", err)
	}
	st, err := c.state.load()
	if err != nil {
		return err
	}
	acct, ok := chooseAccount(c.cfg, st)
	if !ok {
		return fmt.Errorf("weixin: not logged in; run 'nanobot channels login weixin' (or set channels.weixin.token or %s)", EnvToken)
	}
	c.useAccount(acct)
	c.started = true
	runCtx, cancel := context.WithCancel(context.Background())
	c.runCtx, c.cancel = runCtx, cancel
	c.wg.Go(func() {
		c.notify(runCtx, "start")
		c.poll(runCtx)
	})
	source := "saved login"
	if !acct.fromState {
		source = "config"
	}
	c.log.Info("weixin: long polling", "base_url", acct.baseURL, "token", source)
	return nil
}

// useAccount switches to acct, restoring its cursor and context tokens.
// c.mu must be held.
func (c *Channel) useAccount(acct account) {
	c.acct = acct
	c.authRequired = false
	c.contexts = map[string]contextEntry{}
	c.sendCounts = map[string]int{}
	for user, tok := range acct.contexts {
		if strings.TrimSpace(user) != "" && strings.TrimSpace(tok) != "" {
			c.contexts[user] = contextEntry{token: tok}
		}
	}
	c.acct.contexts = nil
}

// Stop stops polling and the typing indicators, waits for them, tells
// iLink the bot went offline (best effort) and saves the state. An
// in-flight getupdates is canceled, so it returns promptly.
func (c *Channel) Stop(ctx context.Context) error {
	c.mu.Lock()
	if !c.started || c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.cancel()
	c.mu.Unlock()
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("weixin: stop: %w", ctx.Err())
	}
	c.notify(ctx, "stop")
	c.saveState()
	return nil
}

// notify sends notifystart or notifystop, which reconcile the bot's
// online state upstream. Failures are logged and ignored.
func (c *Channel) notify(ctx context.Context, action string) {
	token, base, auth := c.session()
	if token == "" || auth {
		return
	}
	err := c.api.do(ctx, request{method: http.MethodPost, base: base, endpoint: "ilink/bot/msg/notify" + action,
		body: notifyRequest{BaseInfo: c.api.info}, token: token, timeout: configTimeout}, nil)
	if err != nil && ctx.Err() == nil {
		c.log.Warn("weixin: notify"+action+" failed (ignored)", "error", err)
	}
}

// session returns the token, its API host and whether iLink rejected it.
func (c *Channel) session() (token, base string, authRequired bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acct.token, c.acct.baseURL, c.authRequired
}

// poll long-polls getupdates until ctx ends. A request that times out is
// repeated at once; other failures wait retryDelay, and backoffDelay
// after three in a row. A stale token stops polling until a new login is
// saved.
func (c *Channel) poll(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		err := c.pollOnce(ctx)
		switch {
		case err == nil:
			failures = 0
			continue
		case ctx.Err() != nil:
			return
		case isAPIError(err, errcodeStaleToken):
			if !c.awaitLogin(ctx) {
				return
			}
			failures = 0
			continue
		case isTimeout(err):
			continue // normal for long polling
		}
		failures++
		wait := c.retryDelay
		if failures >= maxConsecutiveFailures {
			failures = 0
			wait = c.backoffDelay
		}
		c.log.Warn("weixin: getupdates failed; retrying", "error", err, "delay", wait)
		if c.sleep(ctx, wait) != nil {
			return
		}
	}
}

// pollOnce runs one getupdates and handles the messages it returns.
func (c *Channel) pollOnce(ctx context.Context) error {
	c.mu.Lock()
	token, base, cursor, wait := c.acct.token, c.acct.baseURL, c.acct.cursor, c.pollTimeout
	c.mu.Unlock()
	var resp getUpdatesResponse
	err := c.api.do(ctx, request{method: http.MethodPost, base: base, endpoint: "ilink/bot/getupdates",
		body: getUpdatesRequest{GetUpdatesBuf: cursor, BaseInfo: c.api.info}, token: token, timeout: wait + pollSlack}, &resp)
	if err != nil {
		if isAPIError(err, errcodeStaleToken) && c.reloadLogin(token) {
			c.notify(ctx, "start")
			return nil
		}
		return err
	}
	c.mu.Lock()
	if ms := int(resp.LongPollingTimeoutMS); ms > 0 {
		c.pollTimeout = max(time.Duration(ms/1000)*time.Second, minPollTimeout)
	}
	advanced := resp.GetUpdatesBuf != "" && resp.GetUpdatesBuf != c.acct.cursor
	if advanced {
		c.acct.cursor = resp.GetUpdatesBuf
	}
	c.mu.Unlock()
	if advanced {
		c.saveState()
	}
	for _, m := range resp.Msgs {
		c.handle(ctx, m)
	}
	return nil
}

// awaitLogin is entered when iLink rejects the token as stale. Sends
// fail until a login saves a new token, which is checked every
// backoffDelay. It returns false when ctx ends first.
func (c *Channel) awaitLogin(ctx context.Context) bool {
	c.mu.Lock()
	stale := c.acct.token
	c.authRequired = true
	c.mu.Unlock()
	c.log.Error("weixin: the login expired; run 'nanobot channels login weixin --force' (polling resumes once it succeeds)")
	for {
		if c.sleep(ctx, c.backoffDelay) != nil {
			return false
		}
		if c.reloadLogin(stale) {
			c.notify(ctx, "start")
			return true
		}
	}
}

// reloadLogin switches to the token in account.json when a login saved
// one other than stale (nanobot: _reload_replacement_token).
func (c *Channel) reloadLogin(stale string) bool {
	st, err := c.state.load()
	if err != nil {
		c.log.Warn("weixin: cannot read the saved login", "error", err)
		return false
	}
	acct, ok := chooseAccount(c.cfg, st)
	if !ok || acct.token == stale {
		return false
	}
	c.mu.Lock()
	c.useAccount(acct)
	c.mu.Unlock()
	c.log.Info("weixin: loaded the new login")
	return true
}

// handle turns one message from getupdates into an inbound bus message,
// or answers it with a pairing code.
func (c *Channel) handle(ctx context.Context, m wireMessage) {
	if m.MessageType == messageTypeBot {
		return
	}
	from := strings.TrimSpace(m.FromUserID)
	if from == "" {
		return
	}
	id := string(m.MessageID)
	if id == "" {
		id = string(m.Seq)
	}
	if id == "" {
		id = from + "_" + string(m.CreateTimeMS)
	}
	if !c.markSeen(id) {
		return
	}
	if !c.isAllowed(from) {
		c.deny(ctx, from, m.ContextToken)
		return
	}
	if m.ContextToken != "" {
		c.cacheContext(from, m.ContextToken)
		c.saveState()
		c.retryDeferred(ctx, from)
	}
	content := messageText(m.ItemList)
	if content == "" {
		c.log.Debug("weixin: skipping a message without text", "sender", from)
		return
	}
	c.log.Info("weixin: inbound", "sender", from, "items", itemTypes(m.ItemList), "len", len([]rune(content)))
	c.startTyping(from, m.ContextToken)
	in := bus.InboundMessage{
		Channel:   Name,
		SenderID:  from,
		ChatID:    from,
		Content:   content,
		Timestamp: time.Now(),
		Metadata:  map[string]any{"message_id": id},
	}
	if err := c.bus.PublishInbound(ctx, in); err != nil {
		c.stopTyping(from)
		c.log.Warn("weixin: drop inbound message", "sender", from, "error", err)
	}
}

// markSeen records a message ID and reports whether it is new.
func (c *Channel) markSeen(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen[id] {
		return false
	}
	c.seen[id] = true
	c.seenOrder = append(c.seenOrder, id)
	if len(c.seenOrder) > seenMax {
		delete(c.seen, c.seenOrder[0])
		c.seenOrder = c.seenOrder[1:]
	}
	return true
}

// isAllowed mirrors nanobot's BaseChannel.is_allowed: "*" or the user ID
// in allowFrom, or an approval in the pairing store.
func (c *Channel) isAllowed(id string) bool {
	if c.allowAll || c.allow[id] {
		return true
	}
	if c.pairing == nil {
		return false
	}
	ok, err := c.pairing.IsApproved(Name, id)
	if err != nil {
		c.log.Warn("weixin: pairing store unavailable", "error", err)
		return false
	}
	return ok
}

// deny answers a sender who is not allowed: a pairing code in a private
// chat, nothing (but a log line) in a group. The code is sent with the
// message's context_token, which is not cached.
func (c *Channel) deny(ctx context.Context, from, ctxToken string) {
	if strings.HasSuffix(from, "@chatroom") || c.pairing == nil {
		c.log.Warn("weixin: access denied; add the sender to channels.weixin.allowFrom to grant access", "sender", from)
		return
	}
	if ctxToken == "" {
		c.log.Warn("weixin: access denied; cannot send a pairing code without a context_token", "sender", from)
		return
	}
	code, err := c.pairing.Code(Name, from)
	if err != nil {
		c.log.Warn("weixin: pairing store unavailable; dropping the message", "sender", from, "error", err)
		return
	}
	d := c.newDelivery(from, splitMessage(pairing.Reply(code), maxMessageLen))
	for i, chunk := range d.chunks {
		if err := c.sendText(ctx, from, chunk, ctxToken, d.clientID(i)); err != nil {
			c.log.Warn("weixin: send pairing code", "sender", from, "error", err)
			return
		}
	}
	c.log.Info("weixin: sent a pairing code; approve it with 'nanobot pairing approve <code>'", "code", code, "sender", from)
}

// cacheContext stores a user's latest context_token. A new token starts
// a fresh send budget.
func (c *Channel) cacheContext(user, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.contexts[user].token; old != token {
		delete(c.sendCounts, old)
	}
	c.contexts[user] = contextEntry{token: token, at: c.now()}
}

// Send delivers msg as text, split into messages of at most 1800
// characters, using the user's cached context_token (refreshed first
// when it is old). A reply that cannot be sent for lack of a usable
// context_token is held back, up to three per user, and sent when the
// user writes again; Send then returns nil. Media are not supported.
// Progress updates, streamed deltas and thinking (bus.IsStreamed) are
// dropped: the final reply carries the whole text, and thinking is never
// shown in a chat app.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if bus.IsStreamed(msg) {
		return nil // no status display; the typing indicator keeps running
	}
	if msg.ChatID == "" {
		return channels.Permanent(errors.New("weixin: empty chat id"))
	}
	c.stopTyping(msg.ChatID)
	if len(msg.Media) > 0 {
		c.log.Warn("weixin: sending media is not supported; sending the text only", "chat_id", msg.ChatID, "files", len(msg.Media))
	}
	chunks := splitMessage(msg.Content, maxMessageLen)
	if len(chunks) == 0 {
		c.cancelTyping(ctx, msg.ChatID)
		return nil
	}
	token, _, auth := c.session()
	switch {
	case auth:
		return channels.Permanent(errLoginExpired)
	case token == "":
		return errors.New("weixin: not started")
	}
	d := c.resume(msg.ChatID, msg.Content, chunks)
	err := c.deliver(ctx, d)
	c.cancelTyping(ctx, msg.ChatID)
	var q *quotaError
	if errors.As(err, &q) {
		c.addDeferred(d)
		c.log.Warn("weixin: holding the reply until the user writes again", "chat_id", d.chat, "reason", q.reason)
		return nil
	}
	return err
}

// quotaError means the reply cannot be sent with the context_token at
// hand; it can once the user writes again.
type quotaError struct{ reason string }

func (e *quotaError) Error() string { return "weixin: no usable context_token: " + e.reason }

// deliver sends d's remaining chunks. It returns a *quotaError when the
// context_token is missing, used up or rejected; a retryable error
// leaves d resumable (see resume).
func (c *Channel) deliver(ctx context.Context, d *delivery) error {
	ctxToken := c.contextToken(ctx, d.chat)
	if ctxToken == "" {
		c.forget(d)
		return &quotaError{"the user has not written yet"}
	}
	for d.done < len(d.chunks) {
		if !c.reserveSend(ctxToken) {
			c.forget(d)
			return &quotaError{fmt.Sprintf("%d messages sent since the user last wrote", contextMessageBudget)}
		}
		err := c.sendText(ctx, d.chat, d.chunks[d.done], ctxToken, d.clientID(d.done))
		if err == nil {
			d.done++
			continue
		}
		c.unreserveSend(ctxToken)
		switch {
		case isAPIError(err, errcodeContextRestricted):
			c.forget(d)
			return &quotaError{"iLink refused the context_token (expired or quota exhausted)"}
		case isAPIError(err, errcodeStaleToken):
			c.mu.Lock()
			c.authRequired = true
			c.mu.Unlock()
			c.forget(d)
			return channels.Permanent(fmt.Errorf("%w: %w", errLoginExpired, err))
		case !retryableRequest(err):
			c.forget(d)
			return channels.Permanent(err)
		}
		c.keepPartial(d)
		return err
	}
	c.forget(d)
	return nil
}

// sendText sends one text message.
func (c *Channel) sendText(ctx context.Context, to, text, ctxToken, clientID string) error {
	token, base, _ := c.session()
	body := sendMessageRequest{
		Msg: outMessage{
			ToUserID:     to,
			ClientID:     clientID,
			MessageType:  messageTypeBot,
			MessageState: messageStateFinish,
			ItemList:     []wireItem{{Type: itemText, TextItem: &textItem{Text: text}}},
			ContextToken: ctxToken,
		},
		BaseInfo: c.api.info,
	}
	return c.api.do(ctx, request{method: http.MethodPost, base: base, endpoint: "ilink/bot/sendmessage",
		body: body, token: token, timeout: apiTimeout}, nil)
}

// reserveSend counts one message against ctxToken's budget, or reports
// false when the budget is used up.
func (c *Channel) reserveSend(ctxToken string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendCounts[ctxToken] >= contextMessageBudget {
		return false
	}
	c.sendCounts[ctxToken]++
	return true
}

func (c *Channel) unreserveSend(ctxToken string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendCounts[ctxToken] > 0 {
		c.sendCounts[ctxToken]--
	}
}

// contextToken returns the user's cached context_token, refreshed
// through getconfig when it is older than contextTokenMaxAge (nanobot:
// _refresh_context_token_if_stale). A failed refresh keeps the old one.
func (c *Channel) contextToken(ctx context.Context, user string) string {
	c.mu.Lock()
	e := c.contexts[user]
	c.mu.Unlock()
	if e.token == "" || c.now().Sub(e.at) < contextTokenMaxAge {
		return e.token
	}
	token, base, _ := c.session()
	var resp getConfigResponse
	old := e.token
	err := c.api.do(ctx, request{method: http.MethodPost, base: base, endpoint: "ilink/bot/getconfig",
		body: getConfigRequest{UserID: user, ContextToken: &old, BaseInfo: c.api.info}, token: token, timeout: configTimeout}, &resp)
	if err != nil {
		c.log.Warn("weixin: refreshing the context_token failed; using the cached one", "chat_id", user, "error", err)
		return old
	}
	if resp.ContextToken == "" || resp.ContextToken == old {
		return old
	}
	c.cacheContext(user, resp.ContextToken)
	c.saveState()
	return resp.ContextToken
}

// newDelivery returns a delivery of chunks with a fresh id.
func (c *Channel) newDelivery(chat string, chunks []string) *delivery {
	var b [16]byte
	rand.Read(b[:])
	return &delivery{id: hex.EncodeToString(b[:]), chat: chat, chunks: chunks, at: c.now()}
}

// resume returns the partly sent delivery of the same content to chat,
// when the channel manager retries one, or a new delivery.
func (c *Channel) resume(chat, content string, chunks []string) *delivery {
	sum := sha256.Sum256([]byte(chat + "\x00" + content))
	key := hex.EncodeToString(sum[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, d := range c.partial {
		if now.Sub(d.at) > partialTTL {
			delete(c.partial, k)
		}
	}
	if d := c.partial[key]; d != nil && len(d.chunks) == len(chunks) {
		return d
	}
	var b [16]byte
	rand.Read(b[:])
	return &delivery{id: hex.EncodeToString(b[:]), key: key, chat: chat, chunks: chunks, at: now}
}

func (c *Channel) keepPartial(d *delivery) {
	if d.key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d.at = c.now()
	c.partial[d.key] = d
	for len(c.partial) > maxPartials {
		var oldest string
		for k, p := range c.partial {
			if oldest == "" || p.at.Before(c.partial[oldest].at) {
				oldest = k
			}
		}
		delete(c.partial, oldest)
	}
}

func (c *Channel) forget(d *delivery) {
	if d.key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.partial[d.key] == d {
		delete(c.partial, d.key)
	}
}

// addDeferred holds d back until its user writes again, dropping the
// oldest held reply beyond maxDeferredPerChat.
func (c *Channel) addDeferred(d *delivery) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deferred[d.chat] = c.trimDeferred(d.chat, append(c.deferred[d.chat], d))
}

// trimDeferred caps a user's held replies. c.mu must be held.
func (c *Channel) trimDeferred(chat string, list []*delivery) []*delivery {
	for len(list) > maxDeferredPerChat {
		c.log.Warn("weixin: dropped the oldest held reply", "chat_id", chat, "limit", maxDeferredPerChat)
		list = list[1:]
	}
	return list
}

// retryDeferred sends the replies held back for user, oldest first,
// after a message brought a fresh context_token. A reply refused again
// stays held with the ones after it; one that fails otherwise is dropped
// (like nanobot, it is not retried again).
func (c *Channel) retryDeferred(ctx context.Context, user string) {
	c.mu.Lock()
	pending := c.deferred[user]
	delete(c.deferred, user)
	c.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	c.log.Info("weixin: sending held replies", "chat_id", user, "count", len(pending))
	for i, d := range pending {
		err := c.deliver(ctx, d)
		var q *quotaError
		switch {
		case errors.As(err, &q):
			c.mu.Lock()
			c.deferred[user] = c.trimDeferred(user, append(pending[i:len(pending):len(pending)], c.deferred[user]...))
			c.mu.Unlock()
			return
		case err != nil:
			c.log.Warn("weixin: a held reply failed and is dropped", "chat_id", user, "error", err)
		}
	}
}

// startTyping shows "typing…" to user until stopTyping, Stop or
// typingMax. It fetches (and caches) the user's typing ticket first.
func (c *Channel) startTyping(user, ctxToken string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runCtx == nil || c.runCtx.Err() != nil {
		return
	}
	if t := c.typing[user]; t != nil {
		t.cancel()
	}
	ctx, cancel := context.WithTimeout(c.runCtx, typingMax)
	t := &typingTask{cancel: cancel}
	c.typing[user] = t
	c.wg.Go(func() {
		defer func() {
			cancel()
			c.mu.Lock()
			if c.typing[user] == t {
				delete(c.typing, user)
			}
			c.mu.Unlock()
		}()
		ticket := c.typingTicket(ctx, user, ctxToken)
		if ticket == "" {
			return
		}
		for {
			if err := c.sendTyping(ctx, user, ticket, typingStatusTyping); err != nil && ctx.Err() == nil {
				c.log.Debug("weixin: typing indicator", "chat_id", user, "error", err)
			}
			if sleepCtx(ctx, c.typingInterval) != nil {
				return
			}
		}
	})
}

func (c *Channel) stopTyping(user string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.typing[user]; t != nil {
		t.cancel()
		delete(c.typing, user)
	}
}

// cancelTyping clears the indicator on the user's side, when a typing
// ticket is cached. Best effort.
func (c *Channel) cancelTyping(ctx context.Context, user string) {
	c.mu.Lock()
	e := c.tickets[user]
	c.mu.Unlock()
	if e == nil || e.ticket == "" {
		return
	}
	if err := c.sendTyping(ctx, user, e.ticket, typingStatusCancel); err != nil {
		c.log.Debug("weixin: clear typing indicator", "chat_id", user, "error", err)
	}
}

// typingTicket returns the user's typing ticket from getconfig, cached
// for a random part of a day; after a failure it is fetched again with
// backoff, and a previously fetched ticket keeps being used meanwhile
// (nanobot: _get_typing_ticket).
func (c *Channel) typingTicket(ctx context.Context, user, ctxToken string) string {
	now := c.now()
	c.mu.Lock()
	e := c.tickets[user]
	if e != nil && now.Before(e.nextFetch) {
		c.mu.Unlock()
		return e.ticket
	}
	c.mu.Unlock()
	token, base, _ := c.session()
	req := getConfigRequest{UserID: user, BaseInfo: c.api.info}
	if ctxToken != "" {
		req.ContextToken = &ctxToken
	}
	var resp getConfigResponse
	err := c.api.do(ctx, request{method: http.MethodPost, base: base, endpoint: "ilink/bot/getconfig",
		body: req, token: token, timeout: configTimeout}, &resp)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.tickets[user] = &ticketEntry{ticket: resp.TypingTicket,
			nextFetch: now.Add(mrand.N(typingTicketTTL)), retry: ticketRetryMin}
		return resp.TypingTicket
	}
	if e == nil {
		c.tickets[user] = &ticketEntry{nextFetch: now.Add(ticketRetryMin), retry: ticketRetryMin}
		return ""
	}
	e.retry = min(e.retry*2, ticketRetryMax)
	e.nextFetch = now.Add(e.retry)
	return e.ticket
}

func (c *Channel) sendTyping(ctx context.Context, user, ticket string, status int) error {
	token, base, _ := c.session()
	return c.api.do(ctx, request{method: http.MethodPost, base: base, endpoint: "ilink/bot/sendtyping",
		body:  sendTypingRequest{UserID: user, TypingTicket: ticket, Status: status, BaseInfo: c.api.info},
		token: token, timeout: configTimeout}, nil)
}

// saveState writes the token, cursor and context tokens to account.json
// (nanobot: _save_state). It never overwrites a token saved by a login
// since this channel loaded its own, unless the configured token is
// authoritative, nor brings back a saved login that was cleared (the
// channel keeps using it until the gateway restarts).
func (c *Channel) saveState() {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.Lock()
	acct := c.acct
	contexts := make(map[string]string, len(c.contexts))
	for user, e := range c.contexts {
		contexts[user] = e.token
	}
	c.mu.Unlock()
	if acct.token == "" {
		return
	}
	err := c.state.update(func(st *accountState) bool {
		authoritative := c.cfg.Token != "" && acct.token == c.cfg.Token &&
			st.ReplacedConfigTokenSHA256 != tokenFingerprint(c.cfg.Token)
		if st.Token != "" && st.Token != acct.token && !authoritative {
			return false // a login saved a newer token
		}
		if acct.fromState && st.Token == "" {
			return false // the saved login was cleared; do not bring it back
		}
		if st.Token != acct.token {
			st.BotID, st.UserID = "", ""
		}
		if !acct.fromState {
			st.ReplacedConfigTokenSHA256 = ""
		}
		st.Token, st.BaseURL, st.GetUpdatesBuf, st.ContextTokens = acct.token, acct.baseURL, acct.cursor, contexts
		return true
	})
	if err != nil {
		c.log.Warn("weixin: cannot save the state", "error", err)
	}
}

// messageText renders a message's items as text (nanobot:
// _process_message). Media are not downloaded; they appear as
// placeholders such as "[image]", like nanobot's when a download fails.
func messageText(items []wireItem) string {
	var parts []string
	for _, it := range items {
		switch it.Type {
		case itemText:
			if it.TextItem == nil || it.TextItem.Text == "" {
				continue
			}
			parts = append(parts, quoted(it.RefMsg, it.TextItem.Text))
		case itemImage:
			parts = append(parts, "[image]")
		case itemVoice:
			// WeChat's own speech-to-text, when it has one.
			if it.VoiceItem != nil && it.VoiceItem.Text != "" {
				parts = append(parts, "[voice] "+it.VoiceItem.Text)
			} else {
				parts = append(parts, "[voice]")
			}
		case itemFile:
			name := "unknown"
			if it.FileItem != nil && it.FileItem.FileName != "" {
				name = it.FileItem.FileName
			}
			parts = append(parts, "[file: "+name+"]")
		case itemVideo:
			parts = append(parts, "[video]")
		}
	}
	return strings.Join(parts, "\n")
}

// quoted prefixes text with the message it quotes, as
// "[引用: title | text]", unless the quoted message is media.
func quoted(ref *refMsg, text string) string {
	if ref == nil {
		return text
	}
	if mi := ref.MessageItem; mi != nil && isMedia(mi.Type) {
		return text
	}
	var q []string
	if ref.Title != "" {
		q = append(q, ref.Title)
	}
	if mi := ref.MessageItem; mi != nil && mi.TextItem != nil && mi.TextItem.Text != "" {
		q = append(q, mi.TextItem.Text)
	}
	if len(q) == 0 {
		return text
	}
	return "[引用: " + strings.Join(q, " | ") + "]\n" + text
}

func isMedia(t int) bool {
	return t == itemImage || t == itemVoice || t == itemFile || t == itemVideo
}

// itemTypes lists the item types of a message for the log.
func itemTypes(items []wireItem) string {
	s := make([]string, len(items))
	for i, it := range items {
		s[i] = strconv.Itoa(it.Type)
	}
	return strings.Join(s, ",")
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
