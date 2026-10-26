// Package telegram is the Telegram chat channel (nanobot/channels/
// telegram). It talks to the Bot API directly over net/http: getMe
// checks the token at start, getUpdates long-polls for messages, and
// sendMessage delivers replies as plain text in chunks of at most 4096
// characters; files of a reply go up through sendPhoto (images) or
// sendDocument. Inbound photos, and the photo of a replied-to message,
// are downloaded into the workspace (MediaDir) and attached to the turn,
// so vision models see them; voice notes and audio files are saved there
// too and stand in the text as their transcript (with a Transcriber) or
// as "[voice: <path>]". With inlineKeyboards, the message tool's
// buttons become an inline keyboard and a tap comes back as a message
// with the button's label. Senders are checked against allowFrom and the pairing
// store; an unknown sender in a private chat gets a pairing code.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/pairing"
	"github.com/yedamao/nanobot-go/internal/security"
)

const (
	// minBackoff and maxBackoff bound the wait after a failed getUpdates.
	minBackoff = time.Second
	maxBackoff = time.Minute
	// sendAttempts is how often a sendMessage is tried when Telegram
	// answers 429 (nanobot: _SEND_MAX_RETRIES).
	sendAttempts = 3
	// typingInterval is how often "typing" is refreshed while a turn
	// runs; Telegram shows it for about 5 seconds.
	typingInterval = 4 * time.Second
	// typingMax stops the indicator of a turn whose reply never comes.
	typingMax = 10 * time.Minute
	// emptyMessage is what nanobot sends for a message without text; it
	// is never delivered.
	emptyMessage = "[empty message]"
	// replyContextMax caps the quoted text of a replied-to message.
	replyContextMax = 4000
)

// Options are the channel's dependencies besides its config section.
type Options struct {
	// Getenv reads the token override; nil ignores the environment.
	Getenv func(string) string
	// Pairing approves senders by pairing code; nil disables pairing, so
	// only allowFrom grants access.
	Pairing *pairing.Store
	// HTTPClient calls the Bot API; nil builds one from the proxy setting.
	HTTPClient *http.Client
	// Workspace is the agent's workspace directory; inbound photos are
	// saved to its MediaDir. Empty leaves photos out (a caption still
	// goes through).
	Workspace string
	// SSRF checks the http(s) media URLs of outbound messages before the
	// Bot API is asked to fetch them (tools.ssrfWhitelist); nil blocks
	// private, loopback, link-local and metadata addresses.
	SSRF *security.Policy
	// Transcriber turns inbound voice notes and audio files into text;
	// nil leaves them as "[voice: <path>]" in the message.
	Transcriber Transcriber
}

// Transcriber transcribes audio data; name is the saved file's path, whose
// extension tells the format.
type Transcriber interface {
	Transcribe(ctx context.Context, name string, data []byte) (string, error)
}

// Factory returns the channels.Factory for Telegram.
func Factory(opts Options) channels.Factory {
	return func(raw json.RawMessage, deps channels.Deps) (channels.Channel, error) {
		cfg, err := ParseConfig(raw, opts.Getenv)
		if err != nil {
			return nil, err
		}
		return New(cfg, deps, opts)
	}
}

// Channel is a Telegram bot connection.
type Channel struct {
	cfg     Config
	api     *client
	bus     *bus.MessageBus
	log     *slog.Logger
	pairing *pairing.Store
	// workspace is Options.Workspace.
	workspace string
	// ssrf is Options.SSRF.
	ssrf *security.Policy
	// transcriber is Options.Transcriber.
	transcriber Transcriber
	// allow holds the allowFrom entries; usernames are lower-cased and
	// without "@".
	allow    map[string]bool
	allowAll bool

	// sleep waits between attempts (replaced in tests); it returns
	// ctx.Err() if ctx ends first.
	sleep          func(ctx context.Context, d time.Duration) error
	minBackoff     time.Duration
	maxBackoff     time.Duration
	typingInterval time.Duration

	// botID and botUsername are set by Start before polling begins.
	botID       int64
	botUsername string

	mu      sync.Mutex
	started bool
	stopped bool
	runCtx  context.Context
	cancel  context.CancelFunc
	typing  map[int64]*typingTask
	// wg tracks the poller and the typing indicators.
	wg sync.WaitGroup
}

type typingTask struct{ cancel context.CancelFunc }

// New returns a channel for a parsed config (see ParseConfig).
func New(cfg Config, deps channels.Deps, opts Options) (*Channel, error) {
	if deps.Bus == nil {
		return nil, errors.New("telegram: bus is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	hc := opts.HTTPClient
	if hc == nil {
		var err error
		if hc, err = httpClient(cfg); err != nil {
			return nil, err
		}
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Channel{
		cfg:            cfg,
		api:            &client{base: cfg.APIBase, token: cfg.Token, http: hc},
		bus:            deps.Bus,
		log:            log,
		pairing:        opts.Pairing,
		workspace:      opts.Workspace,
		ssrf:           opts.SSRF,
		transcriber:    opts.Transcriber,
		allow:          map[string]bool{},
		sleep:          sleepCtx,
		minBackoff:     minBackoff,
		maxBackoff:     maxBackoff,
		typingInterval: typingInterval,
		typing:         map[int64]*typingTask{},
	}
	for _, a := range cfg.AllowFrom {
		a = strings.TrimSpace(a)
		if a == "*" {
			c.allowAll = true
		}
		c.allow[a] = true
		c.allow[strings.ToLower(strings.TrimPrefix(a, "@"))] = true
	}
	return c, nil
}

// Name implements channels.Channel.
func (c *Channel) Name() string { return Name }

// Start checks the token with getMe, removes a webhook if one is set
// (getUpdates fails while one is) and starts long polling. A rejected
// token is an error; when Telegram cannot be reached (the network is not
// up yet, a 5xx), Start succeeds and the channel keeps trying in the
// background before it polls.
func (c *Channel) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return errors.New("telegram: already started")
	}
	cctx, cancel := context.WithTimeout(ctx, apiTimeout)
	err := c.connect(cctx)
	cancel()
	connected := err == nil
	if err != nil {
		if rejected(err) {
			return err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("telegram: connect: %w", ctx.Err())
		}
		c.log.Warn("telegram: cannot reach the Bot API; retrying in the background", "error", err)
	}
	c.started = true
	runCtx, cancelRun := context.WithCancel(context.Background())
	c.runCtx, c.cancel = runCtx, cancelRun
	c.wg.Go(func() {
		if !connected && !c.reconnect(runCtx) {
			return
		}
		c.poll(runCtx)
	})
	return nil
}

// connect identifies the bot (getMe) and removes a webhook.
func (c *Channel) connect(ctx context.Context) error {
	var me user
	if err := c.api.call(ctx, "getMe", struct{}{}, &me); err != nil {
		if rejected(err) {
			return fmt.Errorf("telegram: the bot token was rejected: %w", err)
		}
		return fmt.Errorf("telegram: connect: %w", err)
	}
	c.botID, c.botUsername = me.ID, me.Username
	if err := c.api.call(ctx, "deleteWebhook", struct{}{}, nil); err != nil {
		c.log.Warn("telegram: could not remove the webhook; polling may fail", "error", err)
	}
	c.log.Info("telegram bot connected", "bot", "@"+me.Username, "id", me.ID)
	return nil
}

// reconnect retries connect with backoff until it succeeds (true), the
// token is rejected or ctx ends (false).
func (c *Channel) reconnect(ctx context.Context) bool {
	backoff := c.minBackoff
	for {
		wait := backoff
		var ae *apiError
		err := c.connect(ctx)
		switch {
		case err == nil:
			return true
		case ctx.Err() != nil:
			return false
		case rejected(err):
			c.log.Error("telegram: stopped", "error", err)
			return false
		case errors.As(err, &ae) && ae.RetryAfter > 0:
			wait = ae.RetryAfter
		default:
			backoff = min(backoff*2, c.maxBackoff)
		}
		c.log.Warn("telegram: cannot reach the Bot API; retrying", "error", err, "delay", wait)
		if c.sleep(ctx, wait) != nil {
			return false
		}
	}
}

// httpClient returns an HTTP client for the Bot API, through cfg.Proxy
// when set.
func httpClient(cfg Config) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Proxy != "" {
		u, err := proxyURL(cfg.Proxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr}, nil
}

// ErrTokenRejected marks a CheckToken error for a token the Bot API
// refused.
var ErrTokenRejected = errors.New("the bot token was rejected")

// CheckToken asks the Bot API who the bot of cfg's token is (getMe), as
// Start does, and returns its username. hc nil uses a client honoring
// cfg.Proxy. A refused token is ErrTokenRejected. Errors never contain
// the token.
func CheckToken(ctx context.Context, cfg Config, hc *http.Client) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}
	if hc == nil {
		var err error
		if hc, err = httpClient(cfg); err != nil {
			return "", err
		}
	}
	api := &client{base: cfg.APIBase, token: cfg.Token, http: hc}
	var me user
	if err := api.call(ctx, "getMe", struct{}{}, &me); err != nil {
		if rejected(err) {
			return "", fmt.Errorf("%w: %v", ErrTokenRejected, err)
		}
		return "", err
	}
	return me.Username, nil
}

// rejected reports whether err says the token is invalid (getMe answers
// 401, or 404 for a malformed token).
func rejected(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.Code == http.StatusUnauthorized || ae.Code == http.StatusNotFound)
}

// Stop stops polling and the typing indicators and waits for them. An
// in-flight getUpdates is canceled, so it returns promptly.
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
		return nil
	case <-ctx.Done():
		return fmt.Errorf("telegram: stop: %w", ctx.Err())
	}
}

// poll long-polls getUpdates until ctx ends, backing off after failures
// and waiting out flood control.
func (c *Channel) poll(ctx context.Context) {
	var offset int64
	backoff := c.minBackoff
	for ctx.Err() == nil {
		var updates []update
		rctx, cancel := context.WithTimeout(ctx, c.cfg.pollWait()+pollSlack)
		err := c.api.call(rctx, "getUpdates", getUpdatesParams{
			Offset:         offset,
			Timeout:        c.cfg.PollTimeout,
			AllowedUpdates: c.allowedUpdates(),
		}, &updates)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait := backoff
			var ae *apiError
			if errors.As(err, &ae) && ae.RetryAfter > 0 {
				wait = ae.RetryAfter
			} else {
				backoff = min(backoff*2, c.maxBackoff)
			}
			c.log.Warn("telegram: getUpdates failed; retrying", "error", err, "delay", wait)
			if c.sleep(ctx, wait) != nil {
				return
			}
			continue
		}
		backoff = c.minBackoff
		for _, u := range updates {
			offset = max(offset, u.UpdateID+1)
			switch {
			case u.Message != nil:
				c.handle(ctx, u.Message)
			case u.CallbackQuery != nil && c.cfg.InlineKeyboards:
				c.handleCallback(ctx, u.CallbackQuery)
			}
		}
	}
}

// allowedUpdates are the update types getUpdates asks for: messages,
// and taps on inline keyboard buttons when inlineKeyboards is on.
func (c *Channel) allowedUpdates() []string {
	if c.cfg.InlineKeyboards {
		return []string{"message", "callback_query"}
	}
	return []string{"message"}
}

// handle turns one incoming message into an inbound bus message, or
// answers it directly (pairing codes, /start).
func (c *Channel) handle(ctx context.Context, m *message) {
	// No sender: a channel post or an anonymous admin. Bots never get
	// other bots' messages through polling, but skip them anyway.
	if m.From == nil || m.From.IsBot || m.Chat.Type == "channel" {
		return
	}
	senderID := senderID(m.From)
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	// Commands go through in groups without a mention, like nanobot's.
	// Group chatter not meant for the bot is ignored before the access
	// check, so it is not logged as denied.
	isCommand := strings.HasPrefix(text, "/")
	if isCommand {
		var ok bool
		if text, ok = c.commandText(text); !ok {
			return // a command for another bot
		}
	} else if !c.addressedToBot(m) {
		return
	}
	if !c.isAllowed(m.From) {
		c.deny(ctx, m, senderID)
		return
	}
	// A photo or voice note whose caption is a command runs the command
	// only.
	hasPhoto := len(m.Photo) > 0 && !isCommand
	k, hasClip := clipOf(m)
	hasClip = hasClip && !isCommand
	if strings.TrimSpace(text) == "" && !hasPhoto && !hasClip {
		c.log.Debug("telegram: skipping a message without text", "chat_id", chatID, "sender", senderID)
		return
	}
	if isCommand && strings.EqualFold(strings.Fields(text)[0], "/start") {
		greeting := fmt.Sprintf("👋 Hi %s! I'm nanobot.\n\nSend me a message and I'll respond!\nType /help to see available commands.", m.From.FirstName)
		if err := c.sendText(ctx, m.Chat.ID, topicID(m), nil, greeting); err != nil {
			c.log.Warn("telegram: send /start greeting", "chat_id", chatID, "error", err)
		}
		return
	}
	// A photo that cannot be fetched leaves its caption, if any, to
	// answer; a photo alone is dropped.
	var media []string
	if hasPhoto {
		if p, err := c.downloadPhoto(ctx, m.Photo); err != nil {
			c.log.Warn("telegram: could not download a photo", "chat_id", chatID, "sender", senderID, "error", err)
			if strings.TrimSpace(text) == "" {
				return
			}
		} else {
			media = []string{p}
		}
	}
	// A voice note or audio file adds its transcript (or path) after the
	// caption, like nanobot's content_parts.
	if hasClip {
		if p, part, err := c.downloadClip(ctx, k); err != nil {
			c.log.Warn("telegram: could not download a "+k.kind+" message", "chat_id", chatID, "sender", senderID, "error", err)
			if strings.TrimSpace(text) == "" {
				return
			}
		} else {
			media = append(media, p)
			text = joinLines(text, part)
		}
	}
	if !isCommand && m.ReplyToMessage != nil {
		// The replied-to photo comes first, like nanobot's reply_media +
		// media.
		quote := c.replyContext(m.ReplyToMessage)
		if r := m.ReplyToMessage; len(r.Photo) > 0 {
			if p, err := c.downloadPhoto(ctx, r.Photo); err != nil {
				c.log.Warn("telegram: could not download the replied-to photo", "chat_id", chatID, "sender", senderID, "error", err)
			} else {
				media = append([]string{p}, media...)
				if quote == "" {
					quote = "[Reply to: photo]"
				}
			}
		} else if rk, ok := clipOf(r); ok {
			if p, part, err := c.downloadClip(ctx, rk); err != nil {
				c.log.Warn("telegram: could not download the replied-to "+rk.kind, "chat_id", chatID, "sender", senderID, "error", err)
			} else {
				media = append([]string{p}, media...)
				if quote == "" {
					quote = "[Reply to: " + part + "]"
				}
			}
		}
		if quote != "" {
			text = joinLines(quote, text)
		}
	}

	in := bus.InboundMessage{
		Channel:   Name,
		SenderID:  senderID,
		ChatID:    chatID,
		Content:   text,
		Media:     media,
		Timestamp: time.Now(),
		Metadata:  metadata(m),
	}
	if t := topicID(m); t != 0 {
		in.SessionKeyOverride = fmt.Sprintf("%s:%s:topic:%d", Name, chatID, t)
	}
	c.startTyping(m.Chat.ID, topicID(m))
	if err := c.bus.PublishInbound(ctx, in); err != nil {
		c.stopTyping(m.Chat.ID)
		c.log.Warn("telegram: drop inbound message", "chat_id", chatID, "error", err)
	}
}

// joinLines joins two parts of a message's content with a newline,
// leaving out an empty one.
func joinLines(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n" + b
}

// deny answers a sender who is not allowed: a pairing code in a private
// chat, nothing (but a log line) in a group.
func (c *Channel) deny(ctx context.Context, m *message, senderID string) {
	if m.Chat.Type != "private" || c.pairing == nil {
		c.log.Warn("telegram: access denied; add the sender to channels.telegram.allowFrom to grant access",
			"sender", senderID, "chat_id", m.Chat.ID)
		return
	}
	code, err := c.pairing.Code(Name, strconv.FormatInt(m.From.ID, 10))
	if err != nil {
		c.log.Warn("telegram: pairing store unavailable; dropping the message", "sender", senderID, "error", err)
		return
	}
	if err := c.sendText(ctx, m.Chat.ID, 0, nil, pairing.Reply(code)); err != nil {
		c.log.Warn("telegram: send pairing code", "sender", senderID, "error", err)
		return
	}
	c.log.Info("telegram: sent a pairing code; approve it with 'nanobot pairing approve <code>'",
		"code", code, "sender", senderID, "chat_id", m.Chat.ID)
}

// isAllowed mirrors nanobot's TelegramChannel.is_allowed: "*", the user
// ID, the username or the legacy "id|username" form in allowFrom, or an
// approval in the pairing store.
func (c *Channel) isAllowed(u *user) bool {
	if c.allowAll {
		return true
	}
	id := strconv.FormatInt(u.ID, 10)
	if c.allow[id] {
		return true
	}
	if u.Username != "" && (c.allow[strings.ToLower(u.Username)] || c.allow[id+"|"+u.Username]) {
		return true
	}
	if c.pairing == nil {
		return false
	}
	// Approvals are stored by user ID; nanobot stored "id|username".
	keys := []string{id}
	if u.Username != "" {
		keys = append(keys, id+"|"+u.Username)
	}
	for _, k := range keys {
		ok, err := c.pairing.IsApproved(Name, k)
		if err != nil {
			c.log.Warn("telegram: pairing store unavailable", "error", err)
			return false
		}
		if ok {
			return true
		}
	}
	return false
}

// addressedToBot reports whether the bot should answer m: always in a
// private chat or under the open group policy; in groups otherwise only
// when m mentions the bot or replies to one of its messages.
func (c *Channel) addressedToBot(m *message) bool {
	if m.Chat.Type == "private" || c.cfg.GroupPolicy == GroupPolicyOpen {
		return true
	}
	if c.mentions(m.Text, m.Entities) || c.mentions(m.Caption, m.CaptionEntities) {
		return true
	}
	r := m.ReplyToMessage
	return r != nil && r.From != nil && r.From.ID == c.botID
}

// mentions reports whether text (with its entities) mentions the bot.
func (c *Channel) mentions(text string, entities []entity) bool {
	if c.botUsername == "" {
		return false
	}
	handle := "@" + strings.ToLower(c.botUsername)
	var units []uint16
	for _, e := range entities {
		switch e.Type {
		case "text_mention":
			if e.User != nil && e.User.ID == c.botID {
				return true
			}
		case "mention":
			if units == nil {
				units = utf16.Encode([]rune(text))
			}
			if e.Offset >= 0 && e.Length >= 0 && e.Offset+e.Length <= len(units) &&
				strings.ToLower(string(utf16.Decode(units[e.Offset:e.Offset+e.Length]))) == handle {
				return true
			}
		}
	}
	return strings.Contains(strings.ToLower(text), handle)
}

// commandText strips the "@bot" suffix of a command ("/new@my_bot" ->
// "/new"). ok is false when the suffix names another bot.
func (c *Channel) commandText(text string) (string, bool) {
	cmd, rest, _ := strings.Cut(text, " ")
	cmd, target, found := strings.Cut(cmd, "@")
	if found && c.botUsername != "" && !strings.EqualFold(target, c.botUsername) {
		return "", false
	}
	if rest != "" {
		return cmd + " " + rest, true
	}
	return cmd, true
}

// replyContext quotes the message m replies to, like nanobot's
// _extract_reply_context.
func (c *Channel) replyContext(r *message) string {
	text := r.Text
	if text == "" {
		text = r.Caption
	}
	if text == "" {
		return ""
	}
	if runes := []rune(text); len(runes) > replyContextMax {
		text = string(runes[:replyContextMax]) + "..."
	}
	switch {
	case r.From != nil && r.From.ID == c.botID:
		return "[Reply to bot: " + text + "]"
	case r.From != nil && r.From.Username != "":
		return "[Reply to @" + r.From.Username + ": " + text + "]"
	case r.From != nil && r.From.FirstName != "":
		return "[Reply to " + r.From.FirstName + ": " + text + "]"
	}
	return "[Reply to: " + text + "]"
}

// Send delivers msg in the topic it came from: its files first (images
// through sendPhoto, anything else as a document; at most
// maxOutboundFiles), then its text, split into messages of at most 4096
// characters. Text that fits in a caption (1024) rides on the first file
// instead. A file that cannot be sent is replaced by a "[Failed to send:
// name]" notice, like nanobot's, so a retry never repeats the files that
// went through. Buttons (msg.Buttons) become an inline keyboard on the
// last message sent when inlineKeyboards is on, and are written under
// the text ("[Yes] [No]") otherwise. With replyToMessage (or an explicit msg.ReplyTo) the
// messages reply to the inbound message. Progress updates, streamed
// deltas, thinking and superseded replies (bus.IsStreamed) are dropped:
// the final reply carries the whole text, thinking is never shown in a
// chat app, and a superseded reply was already sent by the message tool.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if bus.IsStreamed(msg) {
		return nil // no status display; the typing indicator keeps running
	}
	chatID, err := strconv.ParseInt(msg.ChatID, 10, 64)
	if err != nil {
		return channels.Permanent(fmt.Errorf("telegram: invalid chat id %q", msg.ChatID))
	}
	c.stopTyping(chatID)
	text := msg.Content
	if strings.TrimSpace(text) == "" || text == emptyMessage {
		text = ""
	}
	files := msg.Media
	if len(files) > maxOutboundFiles {
		c.log.Warn("telegram: too many files in one reply; sending the first ones", "chat_id", chatID,
			"files", len(files), "limit", maxOutboundFiles)
		files = files[:maxOutboundFiles]
	}
	kb := c.buildInlineKeyboard(msg.Buttons)
	text = buttonsFallback(text, msg, kb)
	if text == "" && len(files) == 0 {
		if kb == nil {
			return nil
		}
		// A keyboard needs a message to sit on.
		text = bus.ButtonsText(msg.Buttons)
	}
	threadID, _ := metaInt(msg.Metadata, "message_thread_id")
	var reply *replyParameters
	if id, err := strconv.ParseInt(msg.ReplyTo, 10, 64); err == nil && id > 0 {
		reply = &replyParameters{MessageID: id, AllowSendingWithoutReply: true}
	} else if id, ok := metaInt(msg.Metadata, "message_id"); ok && c.cfg.ReplyToMessage {
		reply = &replyParameters{MessageID: id, AllowSendingWithoutReply: true}
	}
	caption := ""
	if len(files) > 0 && text != "" && utf16Len(strings.Trim(text, " \n")) <= maxCaptionLen {
		caption, text = strings.Trim(text, " \n"), ""
	}
	// Without text after the files the keyboard goes on the last file
	// (or the notice that replaces it).
	fileKB := kb != nil && text == ""
	for i, p := range files {
		var markup *inlineKeyboardMarkup
		if fileKB && i == len(files)-1 {
			markup = kb
		}
		err := c.sendFile(ctx, chatID, threadID, reply, markup, p, caption)
		if err == nil {
			caption = ""
			continue
		}
		if ctx.Err() != nil {
			return err
		}
		c.log.Warn("telegram: could not send a file", "chat_id", chatID, "file", p, "error", err)
		if err := c.sendMarkup(ctx, chatID, threadID, reply, markup, "[Failed to send: "+mediaName(p)+"]"); err != nil {
			return err
		}
	}
	if caption != "" { // no file went through to carry it
		text = caption
	}
	chunks := splitMessage(text, maxMessageLen)
	for i, chunk := range chunks {
		var markup *inlineKeyboardMarkup
		if !fileKB && i == len(chunks)-1 {
			markup = kb
		}
		if err := c.sendMarkup(ctx, chatID, threadID, reply, markup, chunk); err != nil {
			return err
		}
	}
	return nil
}

// sendText sends one message, waiting out flood control (see retrySend).
func (c *Channel) sendText(ctx context.Context, chatID, threadID int64, reply *replyParameters, text string) error {
	return c.sendMarkup(ctx, chatID, threadID, reply, nil, text)
}

// sendMarkup is sendText with an inline keyboard (nil for none).
func (c *Channel) sendMarkup(ctx context.Context, chatID, threadID int64, reply *replyParameters, markup *inlineKeyboardMarkup, text string) error {
	p := sendMessageParams{ChatID: chatID, Text: text, MessageThreadID: threadID, ReplyParameters: reply, ReplyMarkup: markup}
	return c.retrySend(ctx, chatID, func(ctx context.Context) error {
		return c.api.call(ctx, "sendMessage", p, nil)
	})
}

// retrySend makes one send call, waiting out flood control (429
// retry_after) up to sendAttempts times. Rejections (400, 403: chat not
// found, bot blocked) are Permanent.
func (c *Channel) retrySend(ctx context.Context, chatID int64, send func(context.Context) error) error {
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, apiTimeout)
		err := send(cctx)
		cancel()
		if err == nil {
			return nil
		}
		var ae *apiError
		if !errors.As(err, &ae) {
			return err
		}
		if ae.RetryAfter > 0 && attempt < sendAttempts {
			c.log.Warn("telegram: flood control; waiting", "chat_id", chatID, "delay", ae.RetryAfter, "attempt", attempt)
			if c.sleep(ctx, ae.RetryAfter) != nil {
				return err
			}
			continue
		}
		if ae.Code == http.StatusBadRequest || ae.Code == http.StatusForbidden {
			return channels.Permanent(err)
		}
		return err
	}
}

// startTyping shows "typing…" in a chat until stopTyping, Stop or
// typingMax.
func (c *Channel) startTyping(chatID, threadID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runCtx == nil || c.runCtx.Err() != nil {
		return
	}
	if t := c.typing[chatID]; t != nil {
		t.cancel()
	}
	ctx, cancel := context.WithTimeout(c.runCtx, typingMax)
	t := &typingTask{cancel: cancel}
	c.typing[chatID] = t
	c.wg.Go(func() {
		defer func() {
			cancel()
			c.mu.Lock()
			if c.typing[chatID] == t {
				delete(c.typing, chatID)
			}
			c.mu.Unlock()
		}()
		p := chatActionParams{ChatID: chatID, Action: "typing", MessageThreadID: threadID}
		for {
			if err := c.api.call(ctx, "sendChatAction", p, nil); err != nil && ctx.Err() == nil {
				c.log.Debug("telegram: typing indicator", "chat_id", chatID, "error", err)
			}
			if sleepCtx(ctx, c.typingInterval) != nil {
				return
			}
		}
	})
}

func (c *Channel) stopTyping(chatID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.typing[chatID]; t != nil {
		t.cancel()
		delete(c.typing, chatID)
	}
}

// senderID is nanobot's Telegram sender ID: "id|username", or the bare
// ID for users without a username.
func senderID(u *user) string {
	id := strconv.FormatInt(u.ID, 10)
	if u.Username != "" {
		return id + "|" + u.Username
	}
	return id
}

// topicID returns the forum topic (or private-chat topic) of m, or 0.
func topicID(m *message) int64 {
	if m.IsTopicMessage {
		return m.MessageThreadID
	}
	return 0
}

// metadata is nanobot's _build_message_metadata. It travels with the
// turn and comes back on the reply.
func metadata(m *message) map[string]any {
	md := map[string]any{
		"message_id": m.MessageID,
		"user_id":    m.From.ID,
		"username":   m.From.Username,
		"first_name": m.From.FirstName,
		"is_group":   m.Chat.Type != "private",
		"is_forum":   m.Chat.IsForum,
	}
	if t := topicID(m); t != 0 {
		md["message_thread_id"] = t
	}
	if m.ReplyToMessage != nil {
		md["reply_to_message_id"] = m.ReplyToMessage.MessageID
	}
	return md
}

// metaInt reads an integer from metadata. Metadata that went through
// JSON (a cron job's binding) holds float64 or a string instead of int64.
func metaInt(md map[string]any, key string) (int64, bool) {
	switch v := md[key].(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), v == float64(int64(v))
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
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
