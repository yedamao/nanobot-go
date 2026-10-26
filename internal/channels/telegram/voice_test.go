package telegram

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// oggFixture stands in for a voice note.
var oggFixture = []byte("OggS\x00\x02fixture-opus")

// fakeTranscriber returns text (or err) and records what it was given.
type fakeTranscriber struct {
	text string
	err  error

	mu    sync.Mutex
	names []string
	data  [][]byte
}

func (f *fakeTranscriber) Transcribe(_ context.Context, name string, data []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, name)
	f.data = append(f.data, data)
	return f.text, f.err
}

func voiceDM(id int64, caption string) update {
	u := dm(id, alice, "")
	u.Message.Caption = caption
	u.Message.Voice = &voice{FileID: "vox", FileUniqueID: "AgADvox", Duration: 3, MimeType: "audio/ogg", FileSize: int64(len(oggFixture))}
	return u
}

// checkSavedAudio checks that rel is in the workspace with the fixture
// bytes and mode 0600.
func checkSavedAudio(t *testing.T, h *harness, rel string) {
	t.Helper()
	p := filepath.Join(h.workspace, filepath.FromSlash(rel))
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, oggFixture) {
		t.Errorf("saved %d bytes that differ from the fixture", len(data))
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

const voicePath = "media/telegram/AgADvox.ogg"

func TestVoiceWithoutTranscriber(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("vox", oggFixture)
	h.start(t)
	h.push(voiceDM(1, ""))
	in := h.inbound(t)
	if in.Content != "[voice: "+voicePath+"]" {
		t.Errorf("content = %q", in.Content)
	}
	if !reflect.DeepEqual(in.Media, []string{voicePath}) {
		t.Fatalf("media = %v", in.Media)
	}
	checkSavedAudio(t, h, voicePath)
}

func TestVoiceTranscribed(t *testing.T) {
	h := newHarness(t, nil)
	tr := &fakeTranscriber{text: " remind me at five \n"}
	h.ch.transcriber = tr
	h.f.addFile("vox", oggFixture)
	h.start(t)
	h.push(voiceDM(1, ""))
	in := h.inbound(t)
	if in.Content != "[transcription: remind me at five]" || !reflect.DeepEqual(in.Media, []string{voicePath}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	if len(tr.names) != 1 || tr.names[0] != voicePath || !bytes.Equal(tr.data[0], oggFixture) {
		t.Errorf("transcriber got %v", tr.names)
	}
	checkSavedAudio(t, h, voicePath)

	// The caption comes first, then the transcript.
	h.push(voiceDM(2, "what about this?"))
	if in := h.inbound(t); in.Content != "what about this?\n[transcription: remind me at five]" {
		t.Errorf("content = %q", in.Content)
	}
}

func TestVoiceTranscriptionFails(t *testing.T) {
	for name, tr := range map[string]*fakeTranscriber{
		"error": {err: errors.New("HTTP 500")},
		"empty": {text: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.ch.transcriber = tr
			h.f.addFile("vox", oggFixture)
			h.start(t)
			h.push(voiceDM(1, "hey"))
			in := h.inbound(t)
			if in.Content != "hey\n[voice: "+voicePath+"]" || len(in.Media) != 1 {
				t.Errorf("got content %q media %v", in.Content, in.Media)
			}
			if tr.err != nil && !strings.Contains(h.logs.String(), "transcription failed") {
				t.Errorf("no warning logged:\n%s", h.logs)
			}
		})
	}
}

func TestVoiceDownloadFailure(t *testing.T) {
	h := newHarness(t, nil)
	tr := &fakeTranscriber{text: "never"}
	h.ch.transcriber = tr
	h.start(t) // no file: getFile fails
	h.push(voiceDM(1, "caption"))
	if in := h.inbound(t); in.Content != "caption" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v, want the caption alone", in.Content, in.Media)
	}
	// A voice note alone is dropped.
	h.push(voiceDM(2, ""), dm(3, alice, "next"))
	if in := h.inbound(t); in.Content != "next" {
		t.Errorf("got %q after a voice note alone, want the text after it", in.Content)
	}
	if !strings.Contains(h.logs.String(), "could not download a voice message") {
		t.Errorf("no warning logged:\n%s", h.logs)
	}
	if len(tr.names) != 0 {
		t.Errorf("transcriber called %d times", len(tr.names))
	}
}

func TestVoiceTooLarge(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("vox", oggFixture)
	h.start(t)
	u := voiceDM(1, "big")
	u.Message.Voice.FileSize = maxClipBytes + 1
	h.push(u)
	if in := h.inbound(t); in.Content != "big" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	if n := len(h.f.callsOf("getFile")); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
}

func TestAudioFile(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("song", oggFixture)
	h.start(t)
	u := dm(1, alice, "")
	u.Message.Audio = &audio{FileID: "song", FileUniqueID: "AgADsong", FileName: "track.m4a", MimeType: "audio/mp4"}
	h.push(u)
	in := h.inbound(t)
	want := "media/telegram/AgADsong.m4a"
	if in.Content != "[audio: "+want+"]" || !reflect.DeepEqual(in.Media, []string{want}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	// No MIME type: .mp3, like nanobot's type map.
	u = dm(2, alice, "")
	u.Message.Audio = &audio{FileID: "song", FileUniqueID: "AgADsong2"}
	h.push(u)
	if in := h.inbound(t); !reflect.DeepEqual(in.Media, []string{"media/telegram/AgADsong2.mp3"}) {
		t.Errorf("media = %v", in.Media)
	}
}

func TestReplyToVoice(t *testing.T) {
	h := newHarness(t, nil)
	h.ch.transcriber = &fakeTranscriber{text: "buy milk"}
	h.f.addFile("vox", oggFixture)
	h.f.addFile("large", jpegFixture)
	h.start(t)
	// The replied-to voice note comes before the message's own photo.
	u := photoDM(1, "and this?", photoSizes)
	u.Message.ReplyToMessage = voiceDM(9, "").Message
	h.push(u)
	in := h.inbound(t)
	if in.Content != "[Reply to: [transcription: buy milk]]\nand this?" {
		t.Errorf("content = %q", in.Content)
	}
	if want := []string{voicePath, "media/telegram/AgADuq_Large.jpg"}; !reflect.DeepEqual(in.Media, want) {
		t.Errorf("media = %v, want %v", in.Media, want)
	}
	// A captioned one is quoted by its caption.
	u = dm(2, alice, "summarise")
	u.Message.ReplyToMessage = voiceDM(8, "from Bob").Message
	h.push(u)
	if in := h.inbound(t); in.Content != "[Reply to @alice: from Bob]\nsummarise" || len(in.Media) != 1 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
}

func TestVoiceCaptionCommand(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("vox", oggFixture)
	h.start(t)
	h.push(voiceDM(1, "/new"))
	if in := h.inbound(t); in.Content != "/new" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	if n := len(h.f.callsOf("getFile")); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
}
