package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/security"
)

// albumItems decodes the media array of a sendMediaGroup call.
func albumItems(t *testing.T, c apiCall) []inputMedia {
	t.Helper()
	s, _ := c.Body["media"].(string)
	var items []inputMedia
	if err := json.Unmarshal([]byte(s), &items); err != nil {
		t.Fatalf("media = %q: %v", s, err)
	}
	return items
}

// sendMethods returns the send* methods (but sendChatAction) called so
// far, in order.
func sendMethods(f *fakeAPI) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c.Method, "send") && c.Method != "sendChatAction" {
			out = append(out, c.Method)
		}
	}
	return out
}

func TestSendAlbumPhotos(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.ReplyToMessage = true })
	writeWS(t, h, "out/a.png", pngFixture)
	writeWS(t, h, "out/b.jpg", jpegFixture)
	writeWS(t, h, "out/c.webp", []byte("webp bytes"))
	h.start(t)
	err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "three shots\n",
		Media:    []string{"out/a.png", "out/b.jpg", "out/c.webp"},
		Metadata: map[string]any{"message_id": int64(100), "message_thread_id": int64(7)}})
	if err != nil {
		t.Fatal(err)
	}
	if got := sendMethods(h.f); !reflect.DeepEqual(got, []string{"sendMediaGroup"}) {
		t.Fatalf("sent %v, want one album", got)
	}
	album := h.f.callsOf("sendMediaGroup")[0]
	want := []inputMedia{
		{Type: "photo", Media: "attach://file0", Caption: "three shots", ParseMode: "HTML"},
		{Type: "photo", Media: "attach://file1"},
		{Type: "photo", Media: "attach://file2"},
	}
	if items := albumItems(t, album); !reflect.DeepEqual(items, want) {
		t.Errorf("media = %+v, want %+v", items, want)
	}
	files := album.Files
	if len(files) != 3 || files["file0"].Name != "a.png" || !bytes.Equal(files["file0"].Data, pngFixture) ||
		files["file1"].Name != "b.jpg" || !bytes.Equal(files["file1"].Data, jpegFixture) ||
		files["file2"].Name != "c.webp" || string(files["file2"].Data) != "webp bytes" {
		t.Errorf("uploaded %+v", files)
	}
	if album.Body["chat_id"] != "42" || album.Body["message_thread_id"] != "7" || album.Body["caption"] != nil {
		t.Errorf("fields = %v", album.Body)
	}
	var rp replyParameters
	if err := json.Unmarshal([]byte(album.Body["reply_parameters"].(string)), &rp); err != nil || rp.MessageID != 100 || !rp.AllowSendingWithoutReply {
		t.Errorf("reply_parameters = %v (%v)", album.Body["reply_parameters"], err)
	}

	// Text too long for a caption follows the album.
	long := strings.Repeat("word ", 300)
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: long, Media: []string{"out/a.png", "out/b.jpg"}}); err != nil {
		t.Fatal(err)
	}
	albums, texts := h.f.callsOf("sendMediaGroup"), h.f.callsOf("sendMessage")
	if len(albums) != 2 || albumItems(t, albums[1])[0].Caption != "" {
		t.Errorf("second album = %+v, want one without a caption", albums)
	}
	if len(texts) != 1 || texts[0].Body["text"] != strings.TrimSpace(long) {
		t.Errorf("sendMessage = %+v, want the text after the album", texts)
	}
}

func TestSendAlbumPhotoAndRemoteVideo(t *testing.T) {
	h := newHarness(t, nil)
	h.ch.ssrf = &security.Policy{Resolver: staticResolver{}}
	writeWS(t, h, "a.png", pngFixture)
	h.start(t)
	err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "mixed",
		Media: []string{"a.png", "https://cdn.example/v/clip.mp4?sig=1"}})
	if err != nil {
		t.Fatal(err)
	}
	albums := h.f.callsOf("sendMediaGroup")
	if len(albums) != 1 || len(sendMethods(h.f)) != 1 {
		t.Fatalf("sent %v, want one album", sendMethods(h.f))
	}
	want := []inputMedia{
		{Type: "photo", Media: "attach://file0", Caption: "mixed", ParseMode: "HTML"},
		{Type: "video", Media: "https://cdn.example/v/clip.mp4?sig=1", SupportsStreaming: true},
	}
	if items := albumItems(t, albums[0]); !reflect.DeepEqual(items, want) {
		t.Errorf("media = %+v, want %+v", items, want)
	}
	if len(albums[0].Files) != 1 || albums[0].Files["file0"].Name != "a.png" {
		t.Errorf("uploaded %+v, want the local photo only", albums[0].Files)
	}
}

func TestSendAlbumRuns(t *testing.T) {
	h := newHarness(t, nil)
	for rel, data := range map[string]string{"a.png": "", "b.jpg": "", "c.pdf": "pdf", "d.txt": "txt", "e.ogg": "voice",
		"f.gif": "gif", "g.mp3": "mp3", "h.m4a": "m4a", "i.png": "", "j.mp4": "mp4", "k.pdf": "pdf"} {
		if data == "" {
			writeWS(t, h, rel, pngFixture)
		} else {
			writeWS(t, h, rel, []byte(data))
		}
	}
	h.start(t)
	err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "all of them",
		Media: []string{"a.png", "b.jpg", "c.pdf", "d.txt", "e.ogg", "f.gif", "g.mp3", "h.m4a", "i.png", "j.mp4"}})
	if err != nil {
		t.Fatal(err)
	}
	// Photos and videos share albums, documents and audio files each
	// their own; voice notes and animations never join one.
	want := []string{"sendMediaGroup", "sendMediaGroup", "sendVoice", "sendAnimation", "sendMediaGroup", "sendMediaGroup"}
	if got := sendMethods(h.f); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	albums := h.f.callsOf("sendMediaGroup")
	types := func(c apiCall) []string {
		var out []string
		for _, it := range albumItems(t, c) {
			out = append(out, it.Type)
		}
		return out
	}
	for i, want := range [][]string{{"photo", "photo"}, {"document", "document"}, {"audio", "audio"}, {"photo", "video"}} {
		if got := types(albums[i]); !reflect.DeepEqual(got, want) {
			t.Errorf("album %d = %v, want %v", i, got, want)
		}
	}
	if albumItems(t, albums[0])[0].Caption != "all of them" {
		t.Errorf("caption not on the first album's first item")
	}
	for _, c := range append(albums[1:], h.f.callsOf("sendVoice")[0], h.f.callsOf("sendAnimation")[0]) {
		if c.Body["caption"] != nil || (c.Method == "sendMediaGroup" && albumItems(t, c)[0].Caption != "") {
			t.Errorf("%s has a caption too", c.Method)
		}
	}

	// A photo, a photo and a document: an album, then the document.
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Media: []string{"a.png", "i.png", "k.pdf"}}); err != nil {
		t.Fatal(err)
	}
	want = append(want, "sendMediaGroup", "sendDocument")
	if got := sendMethods(h.f); !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
}

func TestSendVideoNoteNotInAlbum(t *testing.T) {
	h := newHarness(t, nil)
	writeWS(t, h, "a.png", pngFixture)
	writeWS(t, h, "n.video_note.mp4", []byte("round"))
	writeWS(t, h, "b.png", pngFixture)
	writeWS(t, h, "c.png", pngFixture)
	h.start(t)
	err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "split",
		Media: []string{"a.png", "n.video_note.mp4", "b.png", "c.png"}})
	if err != nil {
		t.Fatal(err)
	}
	// The video note splits the photos: a single photo, the note on its
	// own, then an album of the other two.
	want := []string{"sendPhoto", "sendVideoNote", "sendMediaGroup"}
	if got := sendMethods(h.f); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	if p := h.f.callsOf("sendPhoto")[0]; p.Body["caption"] != "split" {
		t.Errorf("sendPhoto caption = %v", p.Body["caption"])
	}
	for _, it := range albumItems(t, h.f.callsOf("sendMediaGroup")[0]) {
		if it.Type != "photo" {
			t.Errorf("album item %+v", it)
		}
	}
}

func TestSendSingleFileNotAnAlbum(t *testing.T) {
	h := newHarness(t, nil)
	writeWS(t, h, "a.png", pngFixture)
	writeWS(t, h, "b.png", pngFixture)
	h.start(t)
	// One file, and two split by one that cannot be read.
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Media: []string{"a.png"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Media: []string{"a.png", "gone.png", "b.png"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"sendPhoto", "sendPhoto", "sendMessage", "sendPhoto"}
	if got := sendMethods(h.f); !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
	if msgs := h.f.callsOf("sendMessage"); msgs[0].Body["text"] != "[Failed to send: gone.png]" {
		t.Errorf("notice = %v", msgs[0].Body["text"])
	}
}

func TestSendAlbumRefusedGoesOneByOne(t *testing.T) {
	h := newHarness(t, nil)
	writeWS(t, h, "a.png", pngFixture)
	writeWS(t, h, "b.mp4", []byte("not really a video"))
	h.f.reply = func(method string, n int) (int, string, bool) {
		switch method {
		case "sendMediaGroup":
			return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: wrong file type"}`, true
		case "sendVideo":
			return http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: wrong file type"}`, true
		}
		return 0, "", false
	}
	h.start(t)
	if err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "pair", Media: []string{"a.png", "b.mp4"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"sendMediaGroup", "sendPhoto", "sendVideo", "sendDocument"}
	if got := sendMethods(h.f); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	photo, doc := h.f.callsOf("sendPhoto")[0], h.f.callsOf("sendDocument")[0]
	if photo.Body["caption"] != "pair" || !bytes.Equal(photo.File, pngFixture) {
		t.Errorf("sendPhoto = %+v, want the caption on the first file", photo)
	}
	if doc.FileName != "b.mp4" || doc.Body["caption"] != nil {
		t.Errorf("sendDocument = %+v", doc)
	}
}

func TestSendAlbumKeepsKeyboardOffAlbum(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.InlineKeyboards = true })
	for _, rel := range []string{"a.png", "b.png", "c.png"} {
		writeWS(t, h, rel, pngFixture)
	}
	h.start(t)
	// The last file carries the keyboard, so it leaves the album.
	err := h.ch.Send(context.Background(), bus.OutboundMessage{ChatID: "42", Content: "Pick",
		Media: []string{"a.png", "b.png", "c.png"}, Buttons: [][]string{{"A", "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := sendMethods(h.f); !reflect.DeepEqual(got, []string{"sendMediaGroup", "sendPhoto"}) {
		t.Fatalf("sent %v", got)
	}
	album, photo := h.f.callsOf("sendMediaGroup")[0], h.f.callsOf("sendPhoto")[0]
	if len(albumItems(t, album)) != 2 || album.Body["reply_markup"] != nil || albumItems(t, album)[0].Caption != "Pick" {
		t.Errorf("album = %+v", album.Body)
	}
	if kb := keyboardOf(t, photo); len(kb) != 1 || len(kb[0]) != 2 || photo.Body["caption"] != nil {
		t.Errorf("last photo = %v", photo.Body)
	}
}
