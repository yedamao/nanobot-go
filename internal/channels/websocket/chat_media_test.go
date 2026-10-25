package websocket

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestChatFrameMedia(t *testing.T) {
	ws, _ := mediaWorkspace(t)
	h := newHarnessWith(t, true, Options{Host: "127.0.0.1", Workspace: ws})
	conn, _ := h.dial(t, "")

	write(t, conn, `{"type":"chat","content":"look","media":[
		{"url":"/ui/media/out/chart.png","name":"chart.png","kind":"image"},
		{"url":"/ui/media/out/my%20file.png","name":"my file.png","kind":"image"},
		{"url":"/ui/media/notes.txt","name":"notes.txt","kind":"file"}]}`)
	in := h.inbound(t)
	want := []string{
		filepath.Join(ws.Root, "out", "chart.png"),
		filepath.Join(ws.Root, "out", "my file.png"),
		filepath.Join(ws.Root, "notes.txt"),
	}
	if in.Content != "look" || !slices.Equal(in.Media, want) {
		t.Errorf("inbound = %q %q, want media %q", in.Content, in.Media, want)
	}

	// Media without text is a message.
	write(t, conn, `{"type":"chat","media":[{"url":"/ui/media/out/chart.png","name":"chart.png","kind":"image"}]}`)
	if in := h.inbound(t); in.Content != "" || len(in.Media) != 1 {
		t.Errorf("media-only inbound = %q %q", in.Content, in.Media)
	}

	rejected := []string{
		"/ui/media/escape.png",                             // symlink out of the workspace
		"/ui/media/../secret.png",                          // dot-dot
		"/ui/media/%2E%2E/secret.png",                      // escaped dot-dot
		"/ui/media/.env",                                   // hidden
		"/ui/media/.git/config.png",                        // hidden directory
		"/ui/media/missing.png",                            // not there
		"/ui/media/out",                                    // not a regular file
		"/etc/passwd",                                      // not a media URL
		"https://example.com/ui/media/x",                   // not same-origin
		"/ui/media/out/chart.png%",                         // bad escape
		"/ui/media/" + filepath.Join(ws.Root, "notes.txt"), // absolute
	}
	for _, u := range rejected {
		write(t, conn, fmt.Sprintf(`{"type":"chat","content":"x","media":[{"url":%q,"name":"x","kind":"file"}]}`, u))
		if f := readFrame(t, conn); f.Type != TypeError || !strings.Contains(f.Content, "not a workspace file") {
			t.Errorf("%s: frame = %+v, want an error", u, f)
		}
	}

	var many []string
	for range MaxChatMedia + 1 {
		many = append(many, `{"url":"/ui/media/notes.txt","name":"n","kind":"file"}`)
	}
	write(t, conn, `{"type":"chat","content":"x","media":[`+strings.Join(many, ",")+`]}`)
	if f := readFrame(t, conn); f.Type != TypeError || !strings.Contains(f.Content, "too many attachments") {
		t.Errorf("too many: frame = %+v", f)
	}
	if n := h.bus.InboundLen(); n != 0 {
		t.Errorf("%d inbound messages published for rejected frames", n)
	}
}

func TestChatFrameMediaWithoutWorkspace(t *testing.T) {
	h := newHarness(t, true)
	conn, _ := h.dial(t, "")
	write(t, conn, `{"type":"chat","content":"x","media":[{"url":"/ui/media/a.png","name":"a.png","kind":"image"}]}`)
	if f := readFrame(t, conn); f.Type != TypeError {
		t.Errorf("frame = %+v, want an error", f)
	}
}
