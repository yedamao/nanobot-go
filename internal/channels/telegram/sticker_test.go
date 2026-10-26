package telegram

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// webpFixture stands in for a sticker.
var webpFixture = []byte("RIFF\x10\x00\x00\x00WEBPVP8 fixture")

func stickerDM(id int64, caption string) update {
	u := dm(id, alice, "")
	u.Message.Caption = caption
	u.Message.Sticker = &sticker{FileID: "stk", FileUniqueID: "AgADstk", FileSize: int64(len(webpFixture))}
	return u
}

const stickerPath = "media/telegram/AgADstk.webp"

func TestSticker(t *testing.T) {
	h := newHarness(t, nil)
	tr := &fakeTranscriber{text: "never"}
	h.ch.transcriber = tr
	h.f.addFile("stk", webpFixture)
	h.start(t)
	h.push(stickerDM(1, ""))
	in := h.inbound(t)
	if in.Content != "[sticker: "+stickerPath+"]" || !reflect.DeepEqual(in.Media, []string{stickerPath}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	p := filepath.Join(h.workspace, filepath.FromSlash(stickerPath))
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, webpFixture) {
		t.Errorf("saved %d bytes that differ from the fixture", len(data))
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	calls := h.f.callsOf("getFile")
	if len(calls) != 1 || calls[0].Body["file_id"] != "stk" {
		t.Errorf("getFile calls = %+v", calls)
	}
	// Stickers are not transcribed.
	if len(tr.names) != 0 {
		t.Errorf("transcriber called with %v", tr.names)
	}

	// A caption, should Telegram send one, comes first.
	h.push(stickerDM(2, "look"))
	if in := h.inbound(t); in.Content != "look\n[sticker: "+stickerPath+"]" || len(in.Media) != 1 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
}

func TestStickerKinds(t *testing.T) {
	for _, tc := range []struct {
		name            string
		animated, video bool
		want            string
	}{
		{"static", false, false, "media/telegram/AgADstk.webp"},
		{"animated", true, false, "media/telegram/AgADstk.tgs"},
		{"video", false, true, "media/telegram/AgADstk.webm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.f.addFile("stk", webpFixture)
			h.start(t)
			u := stickerDM(1, "")
			u.Message.Sticker.IsAnimated = tc.animated
			u.Message.Sticker.IsVideo = tc.video
			h.push(u)
			in := h.inbound(t)
			if in.Content != "[sticker: "+tc.want+"]" || !reflect.DeepEqual(in.Media, []string{tc.want}) {
				t.Errorf("got content %q media %v", in.Content, in.Media)
			}
			if _, err := os.Stat(filepath.Join(h.workspace, filepath.FromSlash(tc.want))); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestStickerDownloadFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t) // no file: getFile fails
	h.push(stickerDM(1, "caption"))
	if in := h.inbound(t); in.Content != "caption" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v, want the caption alone", in.Content, in.Media)
	}
	// A sticker alone is dropped.
	h.push(stickerDM(2, ""), dm(3, alice, "next"))
	if in := h.inbound(t); in.Content != "next" {
		t.Errorf("got %q after a bare sticker, want the text after it", in.Content)
	}
	if !strings.Contains(h.logs.String(), "could not download a sticker message") {
		t.Errorf("no warning logged:\n%s", h.logs)
	}
}

func TestStickerTooLarge(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("stk", webpFixture)
	h.start(t)
	u := stickerDM(1, "")
	u.Message.Sticker.FileSize = maxClipBytes + 1
	h.push(u, dm(2, alice, "next"))
	if in := h.inbound(t); in.Content != "next" {
		t.Errorf("got %q after a large bare sticker, want the text after it", in.Content)
	}
	if n := len(h.f.callsOf("getFile")); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
	if !strings.Contains(h.logs.String(), errTooLarge.Error()) {
		t.Errorf("no size warning logged:\n%s", h.logs)
	}
}

func TestReplyToSticker(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("stk", webpFixture)
	h.start(t)
	u := dm(1, alice, "what does this mean?")
	u.Message.ReplyToMessage = stickerDM(9, "").Message
	h.push(u)
	in := h.inbound(t)
	if in.Content != "[Reply to: [sticker: "+stickerPath+"]]\nwhat does this mean?" {
		t.Errorf("content = %q", in.Content)
	}
	if !reflect.DeepEqual(in.Media, []string{stickerPath}) {
		t.Errorf("media = %v", in.Media)
	}
}

func TestStickerCaptionCommand(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("stk", webpFixture)
	h.start(t)
	h.push(stickerDM(1, "/new"))
	if in := h.inbound(t); in.Content != "/new" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	if n := len(h.f.callsOf("getFile")); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
}
