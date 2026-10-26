package telegram

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/providers"
)

// jpegFixture is a tiny stand-in for a photo: a JPEG signature is all
// type detection needs.
var jpegFixture = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00fixture")

// photoSizes are three sizes of one photo, the largest not last.
var photoSizes = []photoSize{
	{FileID: "small", FileUniqueID: "uq-small", Width: 90, Height: 60, FileSize: 1000},
	{FileID: "large", FileUniqueID: "AgADuq_Large", Width: 1280, Height: 853, FileSize: 90000},
	{FileID: "medium", FileUniqueID: "uq-medium", Width: 320, Height: 213, FileSize: 9000},
}

func photoDM(id int64, caption string, sizes []photoSize) update {
	u := dm(id, alice, "")
	u.Message.Caption = caption
	u.Message.Photo = sizes
	return u
}

func TestLargestPhoto(t *testing.T) {
	if got := largestPhoto(photoSizes).FileID; got != "large" {
		t.Errorf("largest = %s, want large", got)
	}
	tie := []photoSize{{FileID: "a", Width: 10, Height: 10, FileSize: 5}, {FileID: "b", Width: 10, Height: 10, FileSize: 7}}
	if got := largestPhoto(tie).FileID; got != "b" {
		t.Errorf("tie broken by file size: got %s, want b", got)
	}
}

// checkSaved checks that rel is in the workspace with the fixture bytes
// and mode 0600.
func checkSaved(t *testing.T, h *harness, rel string) {
	t.Helper()
	path := filepath.Join(h.workspace, filepath.FromSlash(rel))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, jpegFixture) {
		t.Errorf("saved %d bytes that differ from the fixture", len(data))
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestPhotoWithCaption(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("large", jpegFixture)
	h.start(t)
	h.push(photoDM(1, "what is this?", photoSizes))
	in := h.inbound(t)
	if in.Content != "what is this?" {
		t.Errorf("content = %q", in.Content)
	}
	want := []string{"media/telegram/AgADuq_Large.jpg"}
	if !reflect.DeepEqual(in.Media, want) {
		t.Fatalf("media = %v, want %v", in.Media, want)
	}
	checkSaved(t, h, want[0])
	calls := h.f.callsOf("getFile")
	if len(calls) != 1 || calls[0].Body["file_id"] != "large" {
		t.Errorf("getFile calls = %+v, want one for the largest size", calls)
	}
	// The agent reads it as an image part from the workspace.
	msgs := providers.ExpandMedia([]providers.Message{{Role: providers.RoleUser, Content: in.Content, Media: in.Media}}, h.workspace)
	if parts := msgs[0].Parts; len(parts) != 3 || parts[1].Image == nil || parts[1].Image.MediaType != "image/jpeg" {
		t.Errorf("expanded parts = %+v", parts)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.workspace, "media", "telegram")); len(entries) != 1 {
		t.Errorf("media dir holds %d entries, want 1 (no temp files left)", len(entries))
	}
}

func TestPhotoWithoutCaption(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("large", jpegFixture)
	h.start(t)
	h.push(photoDM(1, "", photoSizes))
	in := h.inbound(t)
	if in.Content != "" || len(in.Media) != 1 {
		t.Fatalf("got content %q media %v, want empty content and one file", in.Content, in.Media)
	}
	checkSaved(t, h, in.Media[0])
}

func TestPhotoDownloadFailure(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness)
		sizes []photoSize // nil means photoSizes
		// downloads is how many file downloads are expected per message.
		downloads int
	}{
		{name: "getFile rejected", setup: func(*harness) {}}, // no such file_id
		{name: "download fails", downloads: 1, setup: func(h *harness) {
			h.f.addFile("large", jpegFixture)
			h.f.reply = func(method string, _ int) (int, string, bool) {
				return http.StatusInternalServerError, "", strings.HasPrefix(method, "file:")
			}
		}},
		{name: "not an image", downloads: 1, setup: func(h *harness) { h.f.addFile("large", []byte("<html>nope</html>")) }},
		{name: "too large to fetch", setup: func(h *harness) { h.f.addFile("large", jpegFixture) },
			sizes: []photoSize{{FileID: "large", FileUniqueID: "big", Width: 4000, Height: 3000, FileSize: maxPhotoBytes + 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			tt.setup(h)
			sizes := tt.sizes
			if sizes == nil {
				sizes = photoSizes
			}
			h.start(t)
			// With a caption, the text still goes through on its own.
			h.push(photoDM(1, "look", sizes))
			in := h.inbound(t)
			if in.Content != "look" || len(in.Media) != 0 {
				t.Errorf("got content %q media %v, want the caption alone", in.Content, in.Media)
			}
			// A photo alone is dropped: the next message is the text after it.
			h.push(photoDM(2, "", sizes), dm(3, alice, "next"))
			if in := h.inbound(t); in.Content != "next" {
				t.Errorf("got %q after a photo alone, want the text after it", in.Content)
			}
			if !strings.Contains(h.logs.String(), "could not download a photo") {
				t.Errorf("no warning logged:\n%s", h.logs)
			}
			if n := len(h.f.callsOf("file:photos/large")); n != 2*tt.downloads {
				t.Errorf("downloads = %d, want %d", n, 2*tt.downloads)
			}
			if tt.sizes != nil && len(h.f.callsOf("getFile")) != 0 {
				t.Error("getFile called for a photo over the limit")
			}
			if entries, _ := os.ReadDir(filepath.Join(h.workspace, "media", "telegram")); len(entries) != 0 {
				t.Errorf("media dir holds %d entries after failures", len(entries))
			}
		})
	}
}

func TestPhotoDownloadSizeCap(t *testing.T) {
	h := newHarness(t, nil)
	// getFile claims nothing about the size; the body itself is too big.
	big := append(append([]byte{}, jpegFixture...), make([]byte, maxPhotoBytes)...)
	h.f.addFile("large", big)
	h.f.reply = func(method string, _ int) (int, string, bool) {
		return http.StatusOK, `{"ok":true,"result":{"file_id":"large","file_path":"photos/large"}}`, method == "getFile"
	}
	h.start(t)
	h.push(photoDM(1, "big one", []photoSize{{FileID: "large", FileUniqueID: "big", Width: 10, Height: 10}}))
	in := h.inbound(t)
	if len(in.Media) != 0 || !strings.Contains(h.logs.String(), "file too large") {
		t.Errorf("media = %v; logs:\n%s", in.Media, h.logs)
	}
}

func TestPhotoInGroups(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("large", jpegFixture)
	h.start(t)
	mentioned := groupMsg(1, alice, "")
	mentioned.Message.Caption = "@" + botName + " what is this?"
	mentioned.Message.CaptionEntities = []entity{{Type: "mention", Offset: 0, Length: len(botName) + 1}}
	mentioned.Message.Photo = photoSizes
	h.push(mentioned)
	in := h.inbound(t)
	if len(in.Media) != 1 || !strings.Contains(in.Content, "what is this?") {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	// Not addressed to the bot: ignored, and nothing is downloaded.
	chatter := groupMsg(2, alice, "")
	chatter.Message.Caption = "holiday pics"
	chatter.Message.Photo = photoSizes
	bare := groupMsg(3, alice, "")
	bare.Message.Photo = photoSizes
	h.push(chatter, bare, dm(4, alice, "next"))
	if in := h.inbound(t); in.Content != "next" {
		t.Errorf("got %q, want the ignored photos skipped", in.Content)
	}
	if n := len(h.f.callsOf("getFile")); n != 1 {
		t.Errorf("getFile calls = %d, want 1", n)
	}
}

func TestPhotoReplyQuotesWithoutCaption(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("large", jpegFixture)
	h.start(t)
	u := photoDM(1, "", photoSizes)
	u.Message.ReplyToMessage = &message{MessageID: 5, From: &user{ID: botID, IsBot: true}, Text: "send me a picture"}
	h.push(u)
	in := h.inbound(t)
	if in.Content != "[Reply to bot: send me a picture]" || len(in.Media) != 1 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
}

func TestPhotoWithoutWorkspace(t *testing.T) {
	h := newHarness(t, nil)
	h.ch.workspace = ""
	h.f.addFile("large", jpegFixture)
	h.start(t)
	h.push(photoDM(1, "caption only", photoSizes))
	if in := h.inbound(t); in.Content != "caption only" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	if n := len(h.f.callsOf("getFile")); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
}

func TestSafeID(t *testing.T) {
	for id, want := range map[string]bool{"AgADuq_Large-1": true, "": false, "../x": false, "a/b": false, "a.b": false} {
		if got := safeID(id); got != want {
			t.Errorf("safeID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestDownloadErrorsHideToken(t *testing.T) {
	f := newFakeAPI(t)
	c := &client{base: f.srv.URL, token: testToken, http: f.srv.Client()}
	f.srv.Close() // connection refused: *url.Error quotes the URL
	_, err := c.download(t.Context(), "photos/x", maxPhotoBytes)
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Errorf("err = %v; want an error without the token", err)
	}
}
