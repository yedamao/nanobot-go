// Package websocket is the browser chat channel. It serves a WebSocket
// endpoint (/ws) and the embedded chat page (internal/webui) on the
// gateway's own HTTP server, which mounts them through channels.Mounter;
// it opens no listener of its own. Each connection is one chat: text
// frames become bus.InboundMessage values and replies for that chat are
// written back as JSON frames. On connect the chat's stored transcript is
// sent as a "history" frame so a reloaded page can show it again, and
// GET /ui/chats lists the stored chats for the page's sidebar.
//
// It borrows only the idea of nanobot's WebSocket channel
// (nanobot/channels/websocket): a bus bridge keyed by chat id. The wire
// protocol is nanobot-go's own and much smaller; see README.md.
//
// There is no browser authentication in this slice, so the channel
// refuses to run unless the gateway listens on loopback, and it answers
// only requests whose Host header names a loopback address (which stops
// DNS rebinding) and whose Origin, if any, is the same host (which stops
// other web pages from opening the socket).
package websocket

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/security"
	"github.com/yedamao/nanobot-go/internal/session"
	"github.com/yedamao/nanobot-go/internal/webui"
)

const (
	// Name is the channel's name: its config section, routing key and
	// session key prefix.
	Name = "websocket"
	// Path is where the WebSocket endpoint is mounted.
	Path = "/ws"
	// SenderID is the InboundMessage.SenderID of every browser message.
	SenderID = "webui"

	// readLimit caps one client frame.
	readLimit = 64 << 10
	// writeTimeout bounds writing one frame to one connection.
	writeTimeout = 10 * time.Second
	// pingInterval is how often an idle connection is pinged, so a
	// vanished browser is noticed.
	pingInterval = 30 * time.Second

	// MaxHistoryMessages caps the messages in a history frame: only the
	// most recent ones are sent.
	MaxHistoryMessages = 200
	// MaxHistoryBytes caps the total content of a history frame; older
	// messages are dropped first. The newest message is sent even when
	// it alone is larger.
	MaxHistoryBytes = 256 << 10

	// ChatsPath lists the stored chats as JSON (see ChatList).
	ChatsPath = "/ui/chats"
	// MaxChats caps the chats listed: only the most recently updated ones.
	MaxChats = 50
	// maxTitleRunes and maxPreviewRunes cap a listed chat's title and
	// preview.
	maxTitleRunes   = 60
	maxPreviewRunes = 80
)

// stopCommand is the agent loop's command for a stop frame
// (agent.CommandStop).
const stopCommand = "/stop"

// chatIDRE is the shape of a client-supplied chat id.
var chatIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Frame types.
const (
	TypeHello   = "hello"
	TypeHistory = "history"
	TypeChat    = "chat"
	// TypeStop asks to stop the chat's reply in progress; it reaches the
	// agent as the /stop command.
	TypeStop    = "stop"
	TypeMessage = "message"
	// TypeProgress carries a transient status of the running turn, such
	// as the tool being run; the next message frame supersedes it.
	TypeProgress = "progress"
	TypeError    = "error"
)

// Frame is one JSON message on the socket, in either direction.
type Frame struct {
	Type    string `json:"type"`
	ChatID  string `json:"chat_id,omitempty"`
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// Messages is set on history frames only, where it is always present
	// (an empty list for a new chat).
	Messages []HistoryMessage `json:"messages,omitzero"`
}

// HistoryMessage is one transcript entry in a history frame.
type HistoryMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Config is the channels.websocket section.
type Config struct {
	Enabled bool `json:"enabled"`
}

// ParseConfig decodes a channels.websocket section strictly.
func ParseConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	if err := channels.DecodeConfig(raw, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Options are the channel's dependencies besides its config section.
type Options struct {
	// Host is the address the gateway's HTTP server listens on. It must
	// be loopback: the channel has no browser authentication yet.
	Host string
	// Sessions loads a chat's stored session for the history frame sent
	// on connect. Pass the session Store rather than the Manager: the
	// store returns a fresh copy, while the Manager's cached *Session
	// belongs to the agent loop's worker. Nil sends an empty history and
	// lists no chats.
	Sessions SessionReader
}

// SessionReader reads stored sessions; session.Store implements it. Load
// returns an error wrapping session.ErrNotFound for an unknown key; List
// returns every session, most recently updated first.
type SessionReader interface {
	Load(key string) (*session.Session, error)
	List() ([]session.Info, error)
}

// Factory returns the channels.Factory for the websocket channel.
func Factory(opts Options) channels.Factory {
	return func(raw json.RawMessage, deps channels.Deps) (channels.Channel, error) {
		cfg, err := ParseConfig(raw)
		if err != nil {
			return nil, err
		}
		return New(cfg, deps, opts)
	}
}

// Channel is the browser chat channel. It implements channels.Channel
// and channels.Mounter.
type Channel struct {
	bus      *bus.MessageBus
	log      *slog.Logger
	sessions SessionReader

	mu      sync.Mutex
	started bool
	stopped bool
	runCtx  context.Context
	cancel  context.CancelFunc
	// conns holds the open connections by chat id; a chat may have
	// several (two tabs on the same chat).
	conns map[string]map[*websocket.Conn]struct{}
	// wg tracks the connection handlers.
	wg sync.WaitGroup
}

// New returns a channel. It fails unless opts.Host is a loopback address.
func New(_ Config, deps channels.Deps, opts Options) (*Channel, error) {
	if deps.Bus == nil {
		return nil, errors.New("websocket: bus is required")
	}
	if !security.IsLoopbackHost(opts.Host) {
		return nil, fmt.Errorf("websocket: refusing to serve the WebUI on %q: it has no authentication yet, so gateway.host must be a loopback address such as 127.0.0.1", opts.Host)
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Channel{bus: deps.Bus, log: log, sessions: opts.Sessions, conns: map[string]map[*websocket.Conn]struct{}{}}, nil
}

// Name implements channels.Channel.
func (c *Channel) Name() string { return Name }

// Mount implements channels.Mounter: GET /ws upgrades to a WebSocket,
// GET /ui/chats lists the stored chats and GET / and /ui/ serve the chat
// page. Requests whose Host is not a loopback name are refused. Until
// Start and after Stop, /ws answers 503.
func (c *Channel) Mount(mux *http.ServeMux) {
	mux.Handle("GET "+Path, loopbackOnly(http.HandlerFunc(c.serveWS)))
	mux.Handle("GET "+ChatsPath, loopbackOnly(http.HandlerFunc(c.serveChats)))
	ui := loopbackOnly(webui.Handler())
	mux.Handle("GET /{$}", ui)
	mux.Handle("GET "+webui.Prefix, ui)
}

// Start begins accepting connections.
func (c *Channel) Start(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return errors.New("websocket: already started")
	}
	c.started = true
	c.runCtx, c.cancel = context.WithCancel(context.Background())
	return nil
}

// Stop closes every connection (status 1001, "going away") and waits for
// their handlers; when ctx ends first the connections are dropped.
func (c *Channel) Stop(ctx context.Context) error {
	c.mu.Lock()
	if !c.started || c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	for _, set := range c.conns {
		for conn := range set {
			c.wg.Go(func() { conn.Close(websocket.StatusGoingAway, "gateway shutting down") })
		}
	}
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		c.cancel()
		return nil
	case <-ctx.Done():
		c.cancel()
		c.mu.Lock()
		for _, set := range c.conns {
			for conn := range set {
				conn.CloseNow()
			}
		}
		c.mu.Unlock()
		return fmt.Errorf("websocket: stop: %w", ctx.Err())
	}
}

// Send writes msg as a "message" frame to every connection of its chat,
// or as a "progress" frame when it is a progress update (bus.IsProgress).
// A chat with no open connection (the tab was closed) is a Permanent
// error for a reply: it is dropped with a log line instead of being
// retried, which would hold up the other chats' replies. A progress
// update for such a chat is dropped silently.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	progress := bus.IsProgress(msg)
	conns := c.connsFor(msg.ChatID)
	if len(conns) == 0 {
		if progress {
			return nil
		}
		return channels.Permanent(fmt.Errorf("websocket: no open connection for chat %q", msg.ChatID))
	}
	frame := Frame{Type: TypeMessage, Role: "assistant", Content: msg.Content}
	if progress {
		frame = Frame{Type: TypeProgress, Content: msg.Content}
	}
	var errs []error
	for _, conn := range conns {
		if err := writeFrame(ctx, conn, frame); err != nil {
			errs = append(errs, err)
			conn.CloseNow() // its handler unregisters it
		}
	}
	if len(errs) == len(conns) {
		return channels.Permanent(fmt.Errorf("websocket: send to chat %q: %w", msg.ChatID, errors.Join(errs...)))
	}
	return nil
}

func (c *Channel) connsFor(chatID string) []*websocket.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*websocket.Conn, 0, len(c.conns[chatID]))
	for conn := range c.conns[chatID] {
		out = append(out, conn)
	}
	return out
}

// serveWS upgrades the request and runs the connection until it closes
// or the channel stops. ?chat_id= resumes a chat; without it the
// connection gets a new random id. Either way the first frame is a
// "hello" carrying the id and the second a "history" frame with the
// chat's stored transcript. A reply racing the connect may arrive before
// the history frame (which then includes it) or, rarely, after it while
// already included; the page tolerates both.
func (c *Channel) serveWS(w http.ResponseWriter, r *http.Request) {
	chatID := r.URL.Query().Get("chat_id")
	if chatID != "" && !chatIDRE.MatchString(chatID) {
		http.Error(w, "chat_id must be 1-64 letters, digits, '-' or '_'", http.StatusBadRequest)
		return
	}
	if chatID == "" {
		chatID = rand.Text()
	}
	c.mu.Lock()
	if !c.started || c.stopped {
		c.mu.Unlock()
		http.Error(w, "websocket channel is not running", http.StatusServiceUnavailable)
		return
	}
	runCtx := c.runCtx
	c.wg.Add(1)
	c.mu.Unlock()
	defer c.wg.Done()

	// Accept rejects an Origin whose host differs from r.Host.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		c.log.Debug("websocket: upgrade failed", "error", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)
	if !c.register(chatID, conn) {
		conn.Close(websocket.StatusGoingAway, "gateway shutting down")
		return
	}
	defer c.unregister(chatID, conn)
	c.log.Info("websocket: connected", "chat_id", chatID)
	defer c.log.Info("websocket: disconnected", "chat_id", chatID)

	ctx, cancel := context.WithCancel(runCtx)
	defer cancel()
	go keepAlive(ctx, conn)
	if err := writeFrame(ctx, conn, Frame{Type: TypeHello, ChatID: chatID}); err != nil {
		return
	}
	if err := writeFrame(ctx, conn, Frame{Type: TypeHistory, Messages: c.history(chatID)}); err != nil {
		return
	}
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			if writeFrame(ctx, conn, Frame{Type: TypeError, Content: "binary frames are not supported"}) != nil {
				return
			}
			continue
		}
		content, problem := parseChat(data)
		if problem != "" {
			if writeFrame(ctx, conn, Frame{Type: TypeError, Content: problem}) != nil {
				return
			}
			continue
		}
		msg := bus.InboundMessage{
			Channel:   Name,
			SenderID:  SenderID,
			ChatID:    chatID,
			Content:   content,
			Timestamp: time.Now(),
		}
		if err := c.bus.PublishInbound(ctx, msg); err != nil {
			return
		}
	}
}

// history returns the chat's stored user and assistant texts, oldest
// first, capped by MaxHistoryMessages and MaxHistoryBytes. It reads the
// whole transcript, including messages consolidation has summarized for
// the model, since the user saw them; tool calls, tool results and system
// messages are left out. A missing session or a failed load (which is
// logged) yields an empty, non-nil list.
func (c *Channel) history(chatID string) []HistoryMessage {
	out := []HistoryMessage{}
	if c.sessions == nil {
		return out
	}
	key := Name + ":" + chatID
	sess, err := c.sessions.Load(key)
	if err != nil {
		if !errors.Is(err, session.ErrNotFound) {
			c.log.Warn("websocket: load history", "session", key, "error", err)
		}
		return out
	}
	return transcript(sess.Messages, MaxHistoryMessages, MaxHistoryBytes)
}

// transcript keeps the user and assistant messages of msgs with non-empty
// text, then the newest of those within maxMessages and maxBytes of
// content; the newest one is kept even when it alone exceeds maxBytes.
func transcript(msgs []providers.Message, maxMessages, maxBytes int) []HistoryMessage {
	out := []HistoryMessage{}
	for _, m := range msgs {
		if (m.Role != providers.RoleUser && m.Role != providers.RoleAssistant) || strings.TrimSpace(m.Content) == "" {
			continue
		}
		out = append(out, HistoryMessage{Role: string(m.Role), Content: m.Content})
	}
	start, size := len(out), 0
	for start > 0 && len(out)-start < maxMessages {
		size += len(out[start-1].Content)
		if size > maxBytes && start < len(out) {
			break
		}
		start--
	}
	return out[start:]
}

// ChatList is the body of GET /ui/chats.
type ChatList struct {
	Chats []ChatInfo `json:"chats"`
}

// ChatInfo is one stored chat in a ChatList.
type ChatInfo struct {
	ChatID    string    `json:"chat_id"`
	UpdatedAt time.Time `json:"updated_at"`
	// Title is the chat's first user message, shortened.
	Title string `json:"title"`
	// Preview is the chat's last user or assistant text, shortened.
	Preview string `json:"preview"`
}

// serveChats answers GET /ui/chats with the stored chats as a ChatList.
func (c *Channel) serveChats(w http.ResponseWriter, _ *http.Request) {
	list, err := c.chats()
	if err != nil {
		c.log.Warn("websocket: list chats", "error", err)
		http.Error(w, "cannot list chats", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	json.NewEncoder(w).Encode(list)
}

// chats lists the stored websocket chats, most recently updated first and
// at most MaxChats of them. Each one is loaded for its title and preview;
// chats with no user or assistant text yet, and chats that fail to load
// (which is logged), are left out.
func (c *Channel) chats() (ChatList, error) {
	out := ChatList{Chats: []ChatInfo{}}
	if c.sessions == nil {
		return out, nil
	}
	infos, err := c.sessions.List()
	if err != nil {
		return out, err
	}
	for _, info := range infos {
		if len(out.Chats) == MaxChats {
			break
		}
		chatID, ok := strings.CutPrefix(info.Key, Name+":")
		if !ok || !chatIDRE.MatchString(chatID) {
			continue
		}
		sess, err := c.sessions.Load(info.Key)
		if err != nil {
			if !errors.Is(err, session.ErrNotFound) {
				c.log.Warn("websocket: load chat", "session", info.Key, "error", err)
			}
			continue
		}
		if entry, ok := chatInfo(chatID, sess); ok {
			out.Chats = append(out.Chats, entry)
		}
	}
	return out, nil
}

// chatInfo summarizes a stored chat, or reports false when it has no
// user or assistant text.
func chatInfo(chatID string, sess *session.Session) (ChatInfo, bool) {
	msgs := transcript(sess.Messages, len(sess.Messages), math.MaxInt)
	if len(msgs) == 0 {
		return ChatInfo{}, false
	}
	info := ChatInfo{ChatID: chatID, UpdatedAt: sess.UpdatedAt, Preview: shorten(msgs[len(msgs)-1].Content, maxPreviewRunes)}
	if i := slices.IndexFunc(msgs, func(m HistoryMessage) bool { return m.Role == string(providers.RoleUser) }); i >= 0 {
		info.Title = shorten(msgs[i].Content, maxTitleRunes)
	}
	return info, true
}

// shorten collapses s's whitespace to single spaces and cuts it to at most
// n runes, ending a cut text with an ellipsis.
func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n-1])) + "…"
}

func (c *Channel) register(chatID string, conn *websocket.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return false
	}
	set := c.conns[chatID]
	if set == nil {
		set = map[*websocket.Conn]struct{}{}
		c.conns[chatID] = set
	}
	set[conn] = struct{}{}
	return true
}

func (c *Channel) unregister(chatID string, conn *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.conns[chatID], conn)
	if len(c.conns[chatID]) == 0 {
		delete(c.conns, chatID)
	}
}

// parseChat returns a client frame's chat text, or why it was rejected.
// A frame that is not a JSON object is taken as plain text; a stop frame
// becomes stopCommand.
func parseChat(data []byte) (content, problem string) {
	text := strings.TrimSpace(string(data))
	if !strings.HasPrefix(text, "{") {
		if text == "" {
			return "", "empty message"
		}
		return text, ""
	}
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return "", "malformed JSON frame"
	}
	switch f.Type {
	case TypeChat:
	case TypeStop:
		return stopCommand, ""
	default:
		return "", fmt.Sprintf("unsupported frame type %q (want %q or %q)", f.Type, TypeChat, TypeStop)
	}
	if strings.TrimSpace(f.Content) == "" {
		return "", "empty message"
	}
	return f.Content, ""
}

func writeFrame(ctx context.Context, conn *websocket.Conn, f Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

// keepAlive pings conn until ctx ends; a failed ping closes it.
func keepAlive(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				conn.CloseNow()
				return
			}
		}
	}
}

// loopbackOnly answers 403 to requests whose Host header is not a
// loopback name, so a DNS-rebound domain cannot reach the UI.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !security.IsLoopbackHost(host) {
			http.Error(w, "the WebUI answers only on loopback addresses", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
