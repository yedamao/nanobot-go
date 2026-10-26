package telegram

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mp4Fixture stands in for a video.
var mp4Fixture = []byte("\x00\x00\x00\x18ftypmp42fixture")

func videoDM(id int64, caption string) update {
	u := dm(id, alice, "")
	u.Message.Caption = caption
	u.Message.Video = &video{FileID: "vid", FileUniqueID: "AgADvid", Width: 640, Height: 360, Duration: 4,
		FileName: "clip.mov", MimeType: "video/quicktime", FileSize: int64(len(mp4Fixture))}
	return u
}

func videoNoteDM(id int64, caption string) update {
	u := dm(id, alice, "")
	u.Message.Caption = caption
	u.Message.VideoNote = &videoNote{FileID: "circle", FileUniqueID: "AgADcircle", Length: 240, Duration: 2, FileSize: int64(len(mp4Fixture))}
	return u
}

const (
	videoPath     = "media/telegram/AgADvid.mov"
	videoNotePath = "media/telegram/AgADcircle.mp4"
)

// checkSavedVideo checks that rel is in the workspace with the fixture
// bytes and mode 0600.
func checkSavedVideo(t *testing.T, h *harness, rel string) {
	t.Helper()
	p := filepath.Join(h.workspace, filepath.FromSlash(rel))
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, mp4Fixture) {
		t.Errorf("saved %d bytes that differ from the fixture", len(data))
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestVideo(t *testing.T) {
	h := newHarness(t, nil)
	tr := &fakeTranscriber{text: "never"}
	h.ch.transcriber = tr
	h.f.addFile("vid", mp4Fixture)
	h.start(t)
	h.push(videoDM(1, ""))
	in := h.inbound(t)
	if in.Content != "[video: "+videoPath+"]" || !reflect.DeepEqual(in.Media, []string{videoPath}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	checkSavedVideo(t, h, videoPath)
	calls := h.f.callsOf("getFile")
	if len(calls) != 1 || calls[0].Body["file_id"] != "vid" {
		t.Errorf("getFile calls = %+v", calls)
	}

	// The caption comes first, then the path.
	h.push(videoDM(2, "what happens here?"))
	if in := h.inbound(t); in.Content != "what happens here?\n[video: "+videoPath+"]" || len(in.Media) != 1 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	// Videos are not transcribed.
	if len(tr.names) != 0 {
		t.Errorf("transcriber called with %v", tr.names)
	}

	// No (or an unknown) MIME type: .mp4, like nanobot's type map.
	u := dm(3, alice, "")
	u.Message.Video = &video{FileID: "vid", FileUniqueID: "AgADvid2", MimeType: "video/x-unknown"}
	h.push(u)
	if in := h.inbound(t); !reflect.DeepEqual(in.Media, []string{"media/telegram/AgADvid2.mp4"}) {
		t.Errorf("media = %v", in.Media)
	}
}

func TestVideoNote(t *testing.T) {
	h := newHarness(t, nil)
	tr := &fakeTranscriber{text: "never"}
	h.ch.transcriber = tr
	h.f.addFile("circle", mp4Fixture)
	h.start(t)
	h.push(videoNoteDM(1, ""))
	in := h.inbound(t)
	if in.Content != "[video_note: "+videoNotePath+"]" || !reflect.DeepEqual(in.Media, []string{videoNotePath}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	checkSavedVideo(t, h, videoNotePath)
	if len(tr.names) != 0 {
		t.Errorf("transcriber called with %v", tr.names)
	}
}

func TestVideoDownloadFailure(t *testing.T) {
	for name, mk := range map[string]func(int64, string) update{"video": videoDM, "video_note": videoNoteDM} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.start(t) // no file: getFile fails
			h.push(mk(1, "caption"))
			if in := h.inbound(t); in.Content != "caption" || len(in.Media) != 0 {
				t.Errorf("got content %q media %v, want the caption alone", in.Content, in.Media)
			}
			// A video alone is dropped.
			h.push(mk(2, ""), dm(3, alice, "next"))
			if in := h.inbound(t); in.Content != "next" {
				t.Errorf("got %q after a bare %s, want the text after it", in.Content, name)
			}
			if !strings.Contains(h.logs.String(), "could not download a "+name+" message") {
				t.Errorf("no warning logged:\n%s", h.logs)
			}
		})
	}
}

func TestVideoTooLarge(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("vid", mp4Fixture)
	h.f.addFile("circle", mp4Fixture)
	h.start(t)
	u := videoDM(1, "big")
	u.Message.Video.FileSize = maxClipBytes + 1
	h.push(u)
	if in := h.inbound(t); in.Content != "big" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	u = videoNoteDM(2, "")
	u.Message.VideoNote.FileSize = maxClipBytes + 1
	h.push(u, dm(3, alice, "next"))
	if in := h.inbound(t); in.Content != "next" {
		t.Errorf("got %q after a large bare video note, want the text after it", in.Content)
	}
	if n := len(h.f.callsOf("getFile")); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
}

func TestReplyToVideo(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("vid", mp4Fixture)
	h.f.addFile("circle", mp4Fixture)
	h.f.addFile("large", jpegFixture)
	h.start(t)
	// The replied-to video note comes before the message's own photo.
	u := photoDM(1, "and this?", photoSizes)
	u.Message.ReplyToMessage = videoNoteDM(9, "").Message
	h.push(u)
	in := h.inbound(t)
	if in.Content != "[Reply to: [video_note: "+videoNotePath+"]]\nand this?" {
		t.Errorf("content = %q", in.Content)
	}
	if want := []string{videoNotePath, "media/telegram/AgADuq_Large.jpg"}; !reflect.DeepEqual(in.Media, want) {
		t.Errorf("media = %v, want %v", in.Media, want)
	}
	// A captioned one is quoted by its caption.
	u = dm(2, alice, "summarise")
	u.Message.ReplyToMessage = videoDM(8, "from Bob").Message
	h.push(u)
	if in := h.inbound(t); in.Content != "[Reply to @alice: from Bob]\nsummarise" || !reflect.DeepEqual(in.Media, []string{videoPath}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
}

func TestVideoCaptionCommand(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("vid", mp4Fixture)
	h.start(t)
	h.push(videoDM(1, "/new"))
	if in := h.inbound(t); in.Content != "/new" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	if n := len(h.f.callsOf("getFile")); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
}
