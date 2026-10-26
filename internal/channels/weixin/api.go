package weixin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Protocol constants, from nanobot's runtime.py (aligned with
// @tencent-weixin/openclaw-weixin 2.4.6).
const (
	channelVersion = "2.4.6"
	appID          = "bot"
	// clientVersion is channelVersion encoded as 0x00MMNNPP.
	clientVersion = 2<<16 | 4<<8 | 6

	itemText  = 1
	itemImage = 2
	itemVoice = 3
	itemFile  = 4
	itemVideo = 5

	// messageTypeBot marks messages the bot sent (1 is a user's).
	messageTypeBot = 2
	// messageStateFinish marks a complete (not streaming) message.
	messageStateFinish = 2

	typingStatusTyping = 1
	typingStatusCancel = 2

	// Business error codes.
	errcodeContextRestricted = -2  // context token expired, quota exhausted
	errcodeInvalidArgument   = -3  // e.g. rejected local_token_list
	errcodeStaleToken        = -14 // the bot token is no longer valid

	// apiTimeout bounds sendmessage; configTimeout getconfig, sendtyping
	// and the lifecycle notifications; qrStatusTimeout get_qrcode_status.
	apiTimeout      = 15 * time.Second
	configTimeout   = 10 * time.Second
	qrStatusTimeout = 60 * time.Second
	// pollSlack is added to the long-poll timeout for the getupdates
	// request (nanobot: poll timeout + 10).
	pollSlack = 10 * time.Second
	// maxResponseBytes caps a response body.
	maxResponseBytes = 8 << 20
	// redacted replaces the bot token wherever it could appear.
	redacted = "<redacted>"
)

// client calls the iLink bot API: JSON over HTTPS, the bot token in the
// Authorization header. It never puts the token in an error.
type client struct {
	http     *http.Client
	routeTag string
	info     baseInfo
}

// baseInfo goes with every authenticated request.
type baseInfo struct {
	ChannelVersion string `json:"channel_version"`
	BotAgent       string `json:"bot_agent"`
}

func newClient(hc *http.Client, routeTag, version string) *client {
	if hc == nil {
		// Like nanobot (trust_env=False), the proxy environment is ignored.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		hc = &http.Client{Transport: tr}
	}
	if version == "" {
		version = "dev"
	}
	return &client{http: hc, routeTag: routeTag,
		info: baseInfo{ChannelVersion: channelVersion, BotAgent: "nanobot-go/" + version + " (go)"}}
}

// apiError is a response with a non-zero ret or errcode.
type apiError struct {
	Endpoint string
	Ret      int
	Errcode  int
	Errmsg   string
}

func (e *apiError) Error() string {
	msg := e.Errmsg
	if msg == "" {
		msg = "no error message"
	}
	return fmt.Sprintf("weixin %s failed (ret=%d, errcode=%d): %s", e.Endpoint, e.Ret, e.Errcode, msg)
}

// is reports whether ret or errcode is code.
func (e *apiError) is(code int) bool { return e.Ret == code || e.Errcode == code }

// httpError is a response with a non-2xx status.
type httpError struct {
	Endpoint string
	Status   int
}

func (e *httpError) Error() string {
	return fmt.Sprintf("weixin %s: HTTP %d", e.Endpoint, e.Status)
}

// retryable reports whether the status may succeed later (nanobot:
// 408, 425, 429 and 5xx).
func (e *httpError) retryable() bool {
	switch e.Status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return e.Status >= 500
}

// isAPIError reports whether err is an apiError with code.
func isAPIError(err error, code int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.is(code)
}

// isTimeout reports whether err is a request deadline or network timeout.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// retryableRequest reports whether a failed request may succeed when
// repeated: network errors, unreadable bodies and retryable statuses, but
// not business errors.
func retryableRequest(err error) bool {
	var ae *apiError
	var he *httpError
	switch {
	case errors.As(err, &ae):
		return false
	case errors.As(err, &he):
		return he.retryable()
	}
	return true
}

// envelope is the error part of every response.
type envelope struct {
	Ret     flexInt `json:"ret"`
	Errcode flexInt `json:"errcode"`
	Errmsg  string  `json:"errmsg"`
}

// flexInt decodes a number, a numeric string or a boolean, like
// nanobot's _response_int; anything else is 0.
type flexInt int

func (n *flexInt) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch v := v.(type) {
	case float64:
		*n = flexInt(v)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(v))
		*n = flexInt(i)
	case bool:
		*n = 0
		if v {
			*n = 1
		}
	default:
		*n = 0
	}
	return nil
}

// flexString decodes a string or a number (message IDs).
type flexString string

func (s *flexString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		*s = flexString(str)
		return nil
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return err
	}
	*s = flexString(n.String())
	return nil
}

// request is one API call.
type request struct {
	method   string // GET or POST
	base     string // API base URL without a trailing slash
	endpoint string // path below base, e.g. ilink/bot/getupdates
	query    url.Values
	body     any    // JSON body of a POST
	token    string // bot token; empty sends no Authorization
	timeout  time.Duration
}

// do sends r and decodes the response into out (when non-nil). A
// response with a non-zero ret or errcode is an *apiError, a non-2xx
// status an *httpError. Errors never contain the token.
func (c *client) do(ctx context.Context, r request, out any) error {
	name := r.endpoint
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name, _, _ = strings.Cut(name, "?")
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	u := r.base + "/" + r.endpoint
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	var body io.Reader
	if r.method == http.MethodPost {
		data, err := json.Marshal(r.body)
		if err != nil {
			return fmt.Errorf("weixin %s: %w", name, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return fmt.Errorf("weixin %s: %w", name, err)
	}
	c.setHeaders(req.Header, r.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return redact(fmt.Errorf("weixin %s: %w", name, err), r.token)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return redact(fmt.Errorf("weixin %s: read response: %w", name, err), r.token)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpError{Endpoint: name, Status: resp.StatusCode}
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("weixin %s: the server returned invalid JSON", name)
	}
	if env.Ret != 0 || env.Errcode != 0 {
		return &apiError{Endpoint: name, Ret: int(env.Ret), Errcode: int(env.Errcode), Errmsg: scrub(env.Errmsg, r.token)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("weixin %s: decode response: %w", name, err)
		}
	}
	return nil
}

// setHeaders sets nanobot's per-request headers (api.ts buildHeaders): a
// fresh random X-WECHAT-UIN every time, the app ID and version, the token
// and the route tag.
func (c *client) setHeaders(h http.Header, token string) {
	h.Set("Content-Type", "application/json")
	h.Set("X-WECHAT-UIN", randomUIN())
	h.Set("AuthorizationType", "ilink_bot_token")
	h.Set("iLink-App-Id", appID)
	h.Set("iLink-App-ClientVersion", strconv.Itoa(clientVersion))
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	if c.routeTag != "" {
		h.Set("SKRouteTag", c.routeTag)
	}
}

// randomUIN is a random uint32 in decimal, base64-encoded.
func randomUIN() string {
	var b [4]byte
	rand.Read(b[:])
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[:])), 10)))
}

// scrub removes token from s.
func scrub(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, redacted)
}

// redact returns err with the token removed from its message. Only a
// context error stays in the chain (for errors.Is), and the timeout bit
// of a net.Error.
func redact(err error, token string) error {
	e := &redactedError{msg: scrub(err.Error(), token)}
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		e.err = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		e.err = context.DeadlineExceeded
	case errors.As(err, &ne) && ne.Timeout():
		e.err = context.DeadlineExceeded
	}
	return e
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// Wire types: only the fields this channel reads or sends.

type qrCodeResponse struct {
	QRCode           string `json:"qrcode"`
	QRCodeImgContent string `json:"qrcode_img_content"`
}

type qrStatusResponse struct {
	Status       string `json:"status"`
	BotToken     string `json:"bot_token"`
	BotID        string `json:"ilink_bot_id"`
	BaseURL      string `json:"baseurl"`
	UserID       string `json:"ilink_user_id"`
	RedirectHost string `json:"redirect_host"`
}

type getUpdatesRequest struct {
	GetUpdatesBuf string   `json:"get_updates_buf"`
	BaseInfo      baseInfo `json:"base_info"`
}

type getUpdatesResponse struct {
	Msgs                 []wireMessage `json:"msgs"`
	GetUpdatesBuf        string        `json:"get_updates_buf"`
	LongPollingTimeoutMS flexInt       `json:"longpolling_timeout_ms"`
}

// qrCodeRequest asks for a login QR code. LocalTokenList holds the bot
// tokens this host already has (nanobot: _local_token_list), so the
// server can recognize an existing binding; it must encode as [] when
// empty.
type qrCodeRequest struct {
	LocalTokenList []string `json:"local_token_list"`
}

// wireMessage is a WeixinMessage.
type wireMessage struct {
	MessageID    flexString `json:"message_id"`
	Seq          flexString `json:"seq"`
	FromUserID   string     `json:"from_user_id"`
	ToUserID     string     `json:"to_user_id"`
	CreateTimeMS flexString `json:"create_time_ms"`
	MessageType  int        `json:"message_type"`
	ContextToken string     `json:"context_token"`
	ItemList     []wireItem `json:"item_list"`
}

type wireItem struct {
	Type      int        `json:"type"`
	TextItem  *textItem  `json:"text_item,omitempty"`
	ImageItem *imageItem `json:"image_item,omitempty"`
	VoiceItem *voiceItem `json:"voice_item,omitempty"`
	FileItem  *fileItem  `json:"file_item,omitempty"`
	VideoItem *videoItem `json:"video_item,omitempty"`
	RefMsg    *refMsg    `json:"ref_msg,omitempty"`
}

type textItem struct {
	Text string `json:"text"`
}

// cdnMedia references an encrypted upload on the WeChat CDN. AESKey is
// the hex key, base64-encoded.
type cdnMedia struct {
	EncryptQueryParam string `json:"encrypt_query_param,omitempty"`
	AESKey            string `json:"aes_key,omitempty"`
	EncryptType       int    `json:"encrypt_type,omitempty"`
}

// imageItem is only sent: inbound images are placeholders.
type imageItem struct {
	Media   *cdnMedia `json:"media,omitempty"`
	MidSize int       `json:"mid_size,omitempty"`
}

type videoItem struct {
	Media     *cdnMedia `json:"media,omitempty"`
	VideoSize int       `json:"video_size,omitempty"`
}

type voiceItem struct {
	Text  string    `json:"text,omitempty"`
	Media *cdnMedia `json:"media,omitempty"`
}

type fileItem struct {
	FileName string    `json:"file_name"`
	Len      string    `json:"len,omitempty"`
	Media    *cdnMedia `json:"media,omitempty"`
}

type refMsg struct {
	Title       string    `json:"title"`
	MessageItem *wireItem `json:"message_item"`
}

type outMessage struct {
	FromUserID   string     `json:"from_user_id"`
	ToUserID     string     `json:"to_user_id"`
	ClientID     string     `json:"client_id"`
	MessageType  int        `json:"message_type"`
	MessageState int        `json:"message_state"`
	ItemList     []wireItem `json:"item_list,omitempty"`
	ContextToken string     `json:"context_token,omitempty"`
}

type sendMessageRequest struct {
	Msg      outMessage `json:"msg"`
	BaseInfo baseInfo   `json:"base_info"`
}

type getUploadURLRequest struct {
	FileKey     string   `json:"filekey"`
	MediaType   int      `json:"media_type"`
	ToUserID    string   `json:"to_user_id"`
	RawSize     int      `json:"rawsize"`
	RawFileMD5  string   `json:"rawfilemd5"`
	FileSize    int      `json:"filesize"`
	NoNeedThumb bool     `json:"no_need_thumb"`
	AESKey      string   `json:"aeskey"`
	BaseInfo    baseInfo `json:"base_info"`
}

type getUploadURLResponse struct {
	UploadParam   string `json:"upload_param"`
	UploadFullURL string `json:"upload_full_url"`
}

type getConfigRequest struct {
	UserID       string   `json:"ilink_user_id"`
	ContextToken *string  `json:"context_token"`
	BaseInfo     baseInfo `json:"base_info"`
}

type getConfigResponse struct {
	TypingTicket string `json:"typing_ticket"`
	ContextToken string `json:"context_token"`
}

type sendTypingRequest struct {
	UserID       string   `json:"ilink_user_id"`
	TypingTicket string   `json:"typing_ticket"`
	Status       int      `json:"status"`
	BaseInfo     baseInfo `json:"base_info"`
}

type notifyRequest struct {
	BaseInfo baseInfo `json:"base_info"`
}
