package telegram

import (
	"reflect"
	"strings"
	"testing"
)

// animationDM is an MPEG-4 animation with the document Telegram sets
// beside it for old clients.
func animationDM(id int64, caption string) update {
	u := dm(id, alice, "")
	u.Message.Caption = caption
	u.Message.Animation = &animation{FileID: "anim", FileUniqueID: "AgADanim", Width: 320, Height: 240, Duration: 3,
		FileName: "dance.mp4", MimeType: "video/mp4", FileSize: int64(len(mp4Fixture))}
	u.Message.Document = &document{FileID: "anim", FileUniqueID: "AgADanim", FileName: "dance.mp4",
		MimeType: "video/mp4", FileSize: int64(len(mp4Fixture))}
	return u
}

const animationPath = "media/telegram/AgADanim.mp4"

func TestAnimation(t *testing.T) {
	h := newHarness(t, nil)
	tr := &fakeTranscriber{text: "never"}
	h.ch.transcriber = tr
	h.f.addFile("anim", mp4Fixture)
	h.start(t)
	// Tagged as an animation, not as the document beside it.
	h.push(animationDM(1, ""))
	in := h.inbound(t)
	if in.Content != "[animation: "+animationPath+"]" || !reflect.DeepEqual(in.Media, []string{animationPath}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	checkSavedVideo(t, h, animationPath)
	calls := h.f.callsOf("getFile")
	if len(calls) != 1 || calls[0].Body["file_id"] != "anim" {
		t.Errorf("getFile calls = %+v", calls)
	}

	// The caption comes first, then the path.
	h.push(animationDM(2, "lol"))
	if in := h.inbound(t); in.Content != "lol\n[animation: "+animationPath+"]" || len(in.Media) != 1 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	// Animations are not transcribed.
	if len(tr.names) != 0 {
		t.Errorf("transcriber called with %v", tr.names)
	}
}

func TestAnimationExt(t *testing.T) {
	for _, tc := range []struct {
		mime, name, want string
	}{
		{"image/gif", "", ".gif"},
		{"video/mp4", "x.gif", ".mp4"},
		{"", "", ".mp4"},
		{"video/x-unknown", "", ".mp4"},
		{"", "funny.gif", ".gif"},
		{"application/octet-stream", "../a/b.webm", ".webm"},
	} {
		k, _ := clipOf(&message{Animation: &animation{MimeType: tc.mime, FileName: tc.name}})
		if got := k.ext(); got != tc.want {
			t.Errorf("ext(%q, %q) = %q, want %q", tc.mime, tc.name, got, tc.want)
		}
	}
}

func TestAnimationGIF(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("gif", mp4Fixture)
	h.start(t)
	u := dm(1, alice, "")
	u.Message.Animation = &animation{FileID: "gif", FileUniqueID: "AgADgif", MimeType: "image/gif"}
	h.push(u)
	const p = "media/telegram/AgADgif.gif"
	if in := h.inbound(t); in.Content != "[animation: "+p+"]" || !reflect.DeepEqual(in.Media, []string{p}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	// Sending it back goes through sendAnimation.
	if m := outboundMediaOf(p); m != asAnimation {
		t.Errorf("outbound media of %s = %v, want sendAnimation", p, m)
	}
}

func TestAnimationDownloadFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t) // no file: getFile fails
	h.push(animationDM(1, "caption"))
	if in := h.inbound(t); in.Content != "caption" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v, want the caption alone", in.Content, in.Media)
	}
	// An animation alone is dropped.
	h.push(animationDM(2, ""), dm(3, alice, "next"))
	if in := h.inbound(t); in.Content != "next" {
		t.Errorf("got %q after a bare animation, want the text after it", in.Content)
	}
	if !strings.Contains(h.logs.String(), "could not download a animation message") {
		t.Errorf("no warning logged:\n%s", h.logs)
	}
}

func TestReplyToAnimation(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("anim", mp4Fixture)
	h.start(t)
	u := dm(1, alice, "what is this?")
	u.Message.ReplyToMessage = animationDM(9, "").Message
	h.push(u)
	in := h.inbound(t)
	if in.Content != "[Reply to: [animation: "+animationPath+"]]\nwhat is this?" || !reflect.DeepEqual(in.Media, []string{animationPath}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
}

func TestAnimationCaptionCommand(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("anim", mp4Fixture)
	h.start(t)
	h.push(animationDM(1, "/new"))
	if in := h.inbound(t); in.Content != "/new" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	if n := len(h.f.callsOf("getFile")); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
}

func TestAlbumWithAnimation(t *testing.T) {
	h := albumHarness(t)
	h.f.addFile("p1", jpegFixture)
	h.f.addFile("anim", mp4Fixture)
	h.start(t)
	a := animationDM(2, "")
	a.Message.MediaGroupID = "g"
	h.push(albumPhoto(1, "g", "look", "p1"), a)
	in := h.inbound(t)
	want := []string{"media/telegram/p1.jpg", animationPath}
	if in.Content != "look\n[animation: "+animationPath+"]" || !reflect.DeepEqual(in.Media, want) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
}
