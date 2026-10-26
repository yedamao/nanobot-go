package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/providers"
	"github.com/yedamao/nanobot-go/internal/security"
)

var pngFixture = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDRfixture")

// writeWS writes data to rel in the harness workspace.
func writeWS(t *testing.T, h *harness, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(h.workspace, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSendPhotoWithCaption(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ReplyToMessage = true })
	writeWS(t, h, "out/chart.png", pngFixture)
	h.start(t)
	err := h.ch.Send(context.Background(), bus.OutboundMessage{Channel: Name, ChatID: "42", Content: "here it is\n",
		Media:    []string{"out/chart.png"},
		Metadata: map[string]any{"message_id": int64(100), "message_thread_id": int64(7)}})
	if err != nil {
		t.Fatal(err)
	}
	photos := h.f.callsOf("sendPhoto")
	if len(photos) != 1 {
		t.Fatalf("sendPhoto calls = %d, want 1", len(photos))
	}
	p := photos[0]
	if !bytes.Equal(p.File, pngFixture) || p.FileName != "chart.png" {
		t.Errorf("uploaded %q (%d bytes), want chart.png with the fixture", p.FileName, len(p.File))
	}
	if p.Body["caption"] != "here it is" || p.Body["chat_id"] != "42" || p.Body["message_thread_id"] != "7" {
		t.Errorf("fields = %v", p.Body)
	}
	var rp replyParameters
	if err := json.Unmarshal([]byte(p.Body["reply_parameters"].(string)), &rp); err != nil || rp.MessageID != 100 || !rp.AllowSendingWithoutReply {
		t.Errorf("reply_parameters = %v (%v)", p.Body["reply_parameters"], err)
	}
	if n := len(h.f.callsOf("sendMessage")); n != 0 {
		t.Errorf("the caption was also sent as %d messages", n)
	}
}

func TestSendPhotoLongTextAndMediaOnly(t *testing.T) {
	h := newHarness(t, nil)
	writeWS(t, h, "a.jpg", jpegFixture)
	h.start(t)
	long := strings.Repeat("word ", 300) // 1500 characters: over a caption
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: long, Media: []string{"a.jpg"}}); err != nil {
		t.Fatal(err)
	}
	photos, texts := h.f.callsOf("sendPhoto"), h.f.callsOf("sendMessage")
	if len(photos) != 1 || photos[0].Body["caption"] != nil {
		t.Fatalf("sendPhoto = %+v, want one without a caption", photos)
	}
	if _, ok := photos[0].Body["reply_parameters"]; ok {
		t.Errorf("reply_parameters without replyToMessage: %v", photos[0].Body)
	}
	if len(texts) != 1 || texts[0].Body["text"] != strings.TrimSpace(long) {
		t.Fatalf("sendMessage = %d calls, want the whole text once", len(texts))
	}

	// Media only: a photo and nothing else.
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: emptyMessage, Media: []string{"a.jpg"}}); err != nil {
		t.Fatal(err)
	}
	photos = h.f.callsOf("sendPhoto")
	if len(photos) != 2 || photos[1].Body["caption"] != nil || len(h.f.callsOf("sendMessage")) != 1 {
		t.Errorf("media only: photos %+v, %d messages", photos, len(h.f.callsOf("sendMessage")))
	}
}

func TestSendDocumentsAndFailures(t *testing.T) {
	h := newHarness(t, nil)
	writeWS(t, h, "notes.txt", []byte("plain notes"))
	abs := filepath.Join(h.workspace, "abs.png")
	writeWS(t, h, "abs.png", pngFixture)
	h.start(t)
	err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "files",
		Media: []string{"gone.png", "notes.txt", abs, "http://127.0.0.1/x.png"}})
	if err != nil {
		t.Fatal(err)
	}
	docs := h.f.callsOf("sendDocument")
	if len(docs) != 1 || string(docs[0].File) != "plain notes" || docs[0].FileName != "notes.txt" {
		t.Errorf("sendDocument = %+v", docs)
	}
	// The caption rides on the first file that went through.
	if docs[0].Body["caption"] != "files" {
		t.Errorf("caption = %v, want it on the document", docs[0].Body["caption"])
	}
	photos := h.f.callsOf("sendPhoto")
	if len(photos) != 1 || !bytes.Equal(photos[0].File, pngFixture) || photos[0].Body["caption"] != nil {
		t.Errorf("sendPhoto = %+v, want the absolute path without a caption", photos)
	}
	var texts []string
	for _, c := range h.f.callsOf("sendMessage") {
		texts = append(texts, c.Body["text"].(string))
	}
	want := []string{"[Failed to send: gone.png]", "[Failed to send: x.png]"}
	if !reflect.DeepEqual(texts, want) {
		t.Errorf("messages = %q, want %q", texts, want)
	}

	// When no file goes through, the text still does.
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "only text", Media: []string{"gone.png"}}); err != nil {
		t.Fatal(err)
	}
	sent := h.f.callsOf("sendMessage")
	if last := sent[len(sent)-1].Body["text"]; last != "only text" {
		t.Errorf("last message = %v, want the text", last)
	}
}

func TestSendPhotoFallbacks(t *testing.T) {
	tooMany := `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`
	h := newHarness(t, nil)
	writeWS(t, h, "a.png", pngFixture)
	h.f.reply = func(method string, n int) (int, string, bool) {
		switch {
		case method == "sendPhoto" && n == 1:
			return 429, tooMany, true
		case method == "sendPhoto" && n == 3:
			return 400, `{"ok":false,"error_code":400,"description":"Bad Request: PHOTO_INVALID_DIMENSIONS"}`, true
		}
		return 0, "", false
	}
	h.start(t)
	// Flood control is waited out.
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Media: []string{"a.png"}}); err != nil {
		t.Fatal(err)
	}
	if got := <-h.sleeps; got != 3*time.Second {
		t.Errorf("waited %v, want 3s", got)
	}
	if n := len(h.f.callsOf("sendPhoto")); n != 2 {
		t.Errorf("sendPhoto attempts = %d, want 2", n)
	}
	// A refused photo goes as a document.
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Media: []string{"a.png"}}); err != nil {
		t.Fatal(err)
	}
	if docs := h.f.callsOf("sendDocument"); len(docs) != 1 || !bytes.Equal(docs[0].File, pngFixture) {
		t.Errorf("sendDocument = %+v, want the refused photo", docs)
	}
	if n := len(h.f.callsOf("sendMessage")); n != 0 {
		t.Errorf("sent %d failure notices", n)
	}
}

func TestSendCapsFiles(t *testing.T) {
	h := newHarness(t, nil)
	writeWS(t, h, "a.png", pngFixture)
	h.start(t)
	media := make([]string, maxOutboundFiles+2)
	for i := range media {
		media[i] = "a.png"
	}
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Media: media}); err != nil {
		t.Fatal(err)
	}
	if n := len(h.f.callsOf("sendPhoto")); n != maxOutboundFiles {
		t.Errorf("sendPhoto calls = %d, want %d", n, maxOutboundFiles)
	}
}

func TestUploadErrorsHideToken(t *testing.T) {
	f := newFakeAPI(t)
	c := &client{base: f.srv.URL, token: testToken, http: f.srv.Client()}
	f.srv.Close()
	err := c.upload(t.Context(), "sendPhoto", map[string]string{"chat_id": "1"}, "photo", "a.png", pngFixture, nil)
	if err == nil || strings.Contains(err.Error(), testToken) || !strings.Contains(err.Error(), "bot<redacted>/sendPhoto") {
		t.Errorf("err = %v; want a redacted error", err)
	}
	// A description quoting the token is scrubbed too.
	f2 := newFakeAPI(t)
	f2.reply = func(string, int) (int, string, bool) {
		return 400, `{"ok":false,"error_code":400,"description":"bad bot` + testToken + `"}`, true
	}
	c2 := &client{base: f2.srv.URL, token: testToken, http: f2.srv.Client()}
	err = c2.upload(t.Context(), "sendPhoto", nil, "photo", "a.png", pngFixture, nil)
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("err = %v; want a scrubbed error", err)
	}
}

// replyPhoto is a photo of Alice's that a message replies to.
var replyPhoto = []photoSize{{FileID: "old", FileUniqueID: "AgADold", Width: 800, Height: 600, FileSize: 500}}

func TestReplyToPhotoAttachesIt(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("old", pngFixture)
	h.f.addFile("large", jpegFixture)
	h.start(t)
	u := dm(1, alice, "what's in it?")
	u.Message.ReplyToMessage = &message{MessageID: 5, From: alice, Chat: u.Message.Chat, Photo: replyPhoto}
	h.push(u)
	in := h.inbound(t)
	if in.Content != "[Reply to: photo]\nwhat's in it?" {
		t.Errorf("content = %q", in.Content)
	}
	if want := []string{"media/telegram/AgADold.png"}; !reflect.DeepEqual(in.Media, want) {
		t.Fatalf("media = %v, want %v", in.Media, want)
	}
	msgs := providers.ExpandMedia([]providers.Message{{Role: providers.RoleUser, Content: in.Content, Media: in.Media}}, h.workspace)
	if parts := msgs[0].Parts; len(parts) != 3 || parts[1].Image == nil || parts[1].Image.MediaType != "image/png" {
		t.Errorf("parts = %+v, want the replied-to photo as an image", parts)
	}

	// A captioned photo replying to a captioned photo: the replied-to one
	// comes first, and its caption is quoted.
	u = photoDM(2, "and this one?", photoSizes)
	u.Message.ReplyToMessage = &message{MessageID: 6, From: alice, Chat: u.Message.Chat, Photo: replyPhoto, Caption: "my cat"}
	h.push(u)
	in = h.inbound(t)
	if in.Content != "[Reply to @alice: my cat]\nand this one?" {
		t.Errorf("content = %q", in.Content)
	}
	if want := []string{"media/telegram/AgADold.png", "media/telegram/AgADuq_Large.jpg"}; !reflect.DeepEqual(in.Media, want) {
		t.Errorf("media = %v, want %v", in.Media, want)
	}

	// The replied-to photo cannot be fetched: the text still goes through.
	u = dm(3, alice, "still there?")
	u.Message.ReplyToMessage = &message{MessageID: 7, From: alice, Chat: u.Message.Chat,
		Photo: []photoSize{{FileID: "missing", FileUniqueID: "AgADmissing"}}}
	h.push(u)
	if in = h.inbound(t); in.Content != "still there?" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
}

func TestReplyToPhotoNotFetchedWhenIgnored(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("old", pngFixture)
	h.start(t)
	carol := &user{ID: 77, FirstName: "Carol", Username: "carol"}
	// Group chatter not addressed to the bot.
	g := groupMsg(1, alice, "nice")
	g.Message.ReplyToMessage = &message{MessageID: 5, From: carol, Chat: g.Message.Chat, Photo: replyPhoto}
	// A sender who is not allowed.
	d := dm(2, carol, "look")
	d.Message.ReplyToMessage = &message{MessageID: 6, From: carol, Chat: d.Message.Chat, Photo: replyPhoto}
	h.push(g, d)
	h.noInbound(t)
	if n := len(h.f.callsOf("getFile")); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
}

// staticResolver answers localhost with loopback and every other host
// with a public address, so remote media URLs are checked without real
// DNS.
type staticResolver struct{}

func (staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if host == "localhost" {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

func TestSendRemoteMediaURLs(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.InlineKeyboards = true })
	h.ch.ssrf = &security.Policy{Resolver: staticResolver{}}
	h.f.reply = func(method string, n int) (int, string, bool) {
		if method == "sendPhoto" && n == 2 { // the second photo is refused
			return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: wrong type of the web page content"}`, true
		}
		return 0, "", false
	}
	h.start(t)
	err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "look",
		Media: []string{"https://cdn.example/img/cat.png?sig=1", "https://cdn.example/odd.jpg",
			"https://cdn.example/report.pdf", "http://169.254.169.254/latest/meta.png", "http://localhost/x.png"},
		Buttons:  [][]string{{"OK"}},
		Metadata: map[string]any{"message_thread_id": int64(7)}})
	if err != nil {
		t.Fatal(err)
	}
	photos, docs := h.f.callsOf("sendPhoto"), h.f.callsOf("sendDocument")
	if len(photos) != 2 || photos[0].Body["photo"] != "https://cdn.example/img/cat.png?sig=1" || photos[0].File != nil ||
		photos[0].Body["caption"] != "look" || photos[0].Body["chat_id"] != "42" || photos[0].Body["message_thread_id"] != "7" {
		t.Errorf("sendPhoto = %+v", photos)
	}
	if photos[1].Body["photo"] != "https://cdn.example/odd.jpg" || photos[1].Body["caption"] != nil {
		t.Errorf("second sendPhoto = %+v", photos[1])
	}
	// The refused photo goes as a document, then the PDF.
	if len(docs) != 2 || docs[0].Body["document"] != "https://cdn.example/odd.jpg" ||
		docs[1].Body["document"] != "https://cdn.example/report.pdf" || docs[1].File != nil {
		t.Fatalf("sendDocument = %+v", docs)
	}
	for _, c := range append(photos, docs...) {
		if _, ok := c.Body["reply_markup"]; ok {
			t.Errorf("%s has the keyboard before the last message: %v", c.Method, c.Body)
		}
	}
	// Blocked targets are never handed to Telegram; their notices go out,
	// the last one with the keyboard.
	var texts []string
	for _, c := range h.f.callsOf("sendMessage") {
		texts = append(texts, c.Body["text"].(string))
	}
	want := []string{"[Failed to send: meta.png]", "[Failed to send: x.png]"}
	if !reflect.DeepEqual(texts, want) {
		t.Errorf("messages = %q, want %q", texts, want)
	}
	if msgs := h.f.callsOf("sendMessage"); msgs[1].Body["reply_markup"] == nil {
		t.Errorf("keyboard not on the last message: %v", msgs[1].Body)
	}
}
