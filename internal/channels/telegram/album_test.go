package telegram

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// albumPhoto is one photo (file_id fid, saved as media/telegram/<fid>.jpg)
// of the album group.
func albumPhoto(id int64, group, caption, fid string) update {
	u := photoDM(id, caption, []photoSize{{FileID: fid, FileUniqueID: fid, Width: 640, Height: 480}})
	u.Message.MediaGroupID = group
	return u
}

func albumHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, nil)
	h.ch.mediaGroupDelay = 50 * time.Millisecond
	return h
}

func TestAlbumPhotos(t *testing.T) {
	h := albumHarness(t)
	for _, id := range []string{"p1", "p2", "p3"} {
		h.f.addFile(id, jpegFixture)
	}
	h.start(t)
	// Telegram may deliver an album over several getUpdates.
	h.push(albumPhoto(1, "g1", "these three", "p1"), albumPhoto(2, "g1", "", "p2"))
	h.push(albumPhoto(3, "g1", "", "p3"))
	in := h.inbound(t)
	want := []string{"media/telegram/p1.jpg", "media/telegram/p2.jpg", "media/telegram/p3.jpg"}
	if in.Content != "these three" || !reflect.DeepEqual(in.Media, want) {
		t.Fatalf("got content %q media %v, want the caption and %v", in.Content, in.Media, want)
	}
	for _, p := range want {
		checkSaved(t, h, p)
	}
	// The turn is the first message's.
	if id, _ := metaInt(in.Metadata, "message_id"); id != 10 {
		t.Errorf("message_id = %v, want 10", in.Metadata["message_id"])
	}
	h.f.waitFor("sendChatAction", 1)
	if n := len(h.f.callsOf("sendChatAction")); n != 1 {
		t.Errorf("%d typing indicators, want 1", n)
	}

	// A new album is a turn of its own (the next inbound message is
	// it, not a leftover of the first); captions on several items are
	// joined, and a photo sent twice is attached once.
	h.push(albumPhoto(4, "g2", "one", "p1"), albumPhoto(5, "g2", "two", "p2"), albumPhoto(6, "g2", "", "p2"))
	in = h.inbound(t)
	if in.Content != "one\ntwo" || !reflect.DeepEqual(in.Media, want[:2]) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
}

func TestAlbumMixed(t *testing.T) {
	h := albumHarness(t)
	h.f.addFile("p1", jpegFixture)
	h.f.addFile("vid", mp4Fixture)
	h.f.addFile("doc", pdfFixture)
	h.start(t)
	v := videoDM(2, "")
	v.Message.MediaGroupID = "g"
	d := documentDM(3, "")
	d.Message.MediaGroupID = "g"
	h.push(albumPhoto(1, "g", "trip", "p1"), v, d)
	in := h.inbound(t)
	want := []string{"media/telegram/p1.jpg", videoPath, documentPath}
	if in.Content != "trip\n[video: "+videoPath+"]\n[file: "+documentPath+"]" || !reflect.DeepEqual(in.Media, want) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
}

func TestAlbumWaitsForTheDelay(t *testing.T) {
	h := albumHarness(t)
	h.ch.mediaGroupDelay = 300 * time.Millisecond
	h.f.addFile("p1", jpegFixture)
	h.f.addFile("p2", jpegFixture)
	h.start(t)
	start := time.Now()
	h.push(albumPhoto(1, "g", "", "p1"))
	time.Sleep(100 * time.Millisecond)
	if n := h.bus.InboundLen(); n != 0 {
		t.Fatalf("%d inbound messages before the delay", n)
	}
	h.push(albumPhoto(2, "g", "", "p2"))
	in := h.inbound(t)
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("flushed after %v, want at least the delay", d)
	}
	if len(in.Media) != 2 || in.Content != "" {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
}

func TestAlbumDownloadFailure(t *testing.T) {
	h := albumHarness(t)
	h.f.addFile("p1", jpegFixture) // p2 is missing
	h.start(t)
	h.push(albumPhoto(1, "g1", "", "p1"), albumPhoto(2, "g1", "", "p2"))
	if in := h.inbound(t); !reflect.DeepEqual(in.Media, []string{"media/telegram/p1.jpg"}) {
		t.Errorf("media = %v, want the photo that came through", in.Media)
	}
	// The caption of a failed item still goes through.
	h.push(albumPhoto(3, "g2", "look", "p2"), albumPhoto(4, "g2", "", "p3"))
	if in := h.inbound(t); in.Content != "look" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v, want the caption alone", in.Content, in.Media)
	}
	// Nothing came through: the album is dropped.
	h.push(albumPhoto(5, "g3", "", "p2"), albumPhoto(6, "g3", "", "p3"))
	time.Sleep(150 * time.Millisecond)
	h.push(dm(7, alice, "next"))
	if in := h.inbound(t); in.Content != "next" {
		t.Errorf("got %q after a failed album, want the text after it", in.Content)
	}
}

func TestAlbumDoesNotDelayOtherMessages(t *testing.T) {
	h := albumHarness(t)
	h.ch.mediaGroupDelay = time.Hour
	h.f.addFile("large", jpegFixture)
	h.f.addFile("p1", jpegFixture)
	h.start(t)
	h.push(albumPhoto(1, "g", "", "p1"), photoDM(2, "single", photoSizes), dm(3, alice, "hi"))
	if in := h.inbound(t); in.Content != "single" || len(in.Media) != 1 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	if in := h.inbound(t); in.Content != "hi" {
		t.Errorf("content = %q", in.Content)
	}
	// Stop drops the pending album without waiting for it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.ch.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if n := h.bus.InboundLen(); n != 0 {
		t.Errorf("%d inbound messages after Stop", n)
	}
}

func TestAlbumsOfDifferentChats(t *testing.T) {
	h := albumHarness(t)
	h.f.addFile("p1", jpegFixture)
	h.f.addFile("p2", jpegFixture)
	h.start(t)
	g := albumPhoto(2, "g", "in the group", "p2")
	g.Message.Chat = chat{ID: -100500, Type: "supergroup"}
	g.Message.Caption = "@" + botName + " in the group"
	h.push(albumPhoto(1, "g", "private", "p1"), g)
	got := map[string]int{}
	for range 2 {
		in := h.inbound(t)
		got[in.ChatID] = len(in.Media)
	}
	if !reflect.DeepEqual(got, map[string]int{"42": 1, "-100500": 1}) {
		t.Errorf("media per chat = %v", got)
	}
}
