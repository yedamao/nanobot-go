// Package websocket is the browser chat channel. It serves a WebSocket
// endpoint (/ws) and the embedded chat page (internal/webui) on the
// gateway's own HTTP server, which mounts them through channels.Mounter;
// it opens no listener of its own. Each connection is one chat: text
// frames become bus.InboundMessage values and replies for that chat are
// written back as JSON frames.
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
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/security"
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
)

// chatIDRE is the shape of a client-supplied chat id.
var chatIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Frame types.
const (
	TypeHello   = "hello"
	TypeChat    = "chat"
	TypeMessage = "message"
	TypeError   = "error"
)

// Frame is one JSON message on the socket, in either direction.
type Frame struct {
	Type    string `json:"type"`
	ChatID  string `json:"chat_id,omitempty"`
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
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
	bus *bus.MessageBus
	log *slog.Logger

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
	return &Channel{bus: deps.Bus, log: log, conns: map[string]map[*websocket.Conn]struct{}{}}, nil
}

// Name implements channels.Channel.
func (c *Channel) Name() string { return Name }

// Mount implements channels.Mounter: GET /ws upgrades to a WebSocket and
// GET / and /ui/ serve the chat page. Requests whose Host is not a
// loopback name are refused. Until Start and after Stop, /ws answers 503.
func (c *Channel) Mount(mux *http.ServeMux) {
	mux.Handle("GET "+Path, loopbackOnly(http.HandlerFunc(c.serveWS)))
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

// Send writes msg as a "message" frame to every connection of its chat.
// A chat with no open connection (the tab was closed) is a Permanent
// error: the reply is dropped with a log line instead of being retried,
// which would hold up the other chats' replies.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	conns := c.connsFor(msg.ChatID)
	if len(conns) == 0 {
		return channels.Permanent(fmt.Errorf("websocket: no open connection for chat %q", msg.ChatID))
	}
	frame := Frame{Type: TypeMessage, Role: "assistant", Content: msg.Content}
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
// "hello" carrying the id.
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
// A frame that is not a JSON object is taken as plain text.
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
	if f.Type != TypeChat {
		return "", fmt.Sprintf("unsupported frame type %q (want %q)", f.Type, TypeChat)
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
