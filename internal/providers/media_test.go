package providers

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tinyPNG is enough of a PNG for content sniffing.
var tinyPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSplitMedia(t *testing.T) {
	content, images := SplitMedia("look", []string{"/w/a.PNG", "/w/notes.pdf", "b.webp", "/w/x.svg"})
	if !slices.Equal(images, []string{"/w/a.PNG", "b.webp"}) {
		t.Errorf("images = %q", images)
	}
	if want := "look\n\n[Attachment: /w/notes.pdf]\n[Attachment: /w/x.svg]"; content != want {
		t.Errorf("content = %q, want %q", content, want)
	}
	if content, _ := SplitMedia("", []string{"r.txt"}); content != "[Attachment: r.txt]" {
		t.Errorf("media-only content = %q", content)
	}
	if content, images := SplitMedia("hi", nil); content != "hi" || images != nil {
		t.Errorf("no media = %q, %q", content, images)
	}
}

func TestExpandMedia(t *testing.T) {
	dir := t.TempDir()
	png := writeFile(t, dir, "a.png", tinyPNG)
	writeFile(t, dir, "rel.png", tinyPNG)
	fake := writeFile(t, dir, "fake.png", []byte("not an image"))
	big := writeFile(t, dir, "big.png", append(slices.Clone(tinyPNG), make([]byte, MaxImageBytes)...))

	msgs := []Message{
		{Role: RoleSystem, Content: "sys", Media: []string{png}}, // only user messages expand
		{Role: RoleUser, Content: "what is this?", Media: []string{png, "rel.png", fake, big, filepath.Join(dir, "missing.png")}},
	}
	out := ExpandMedia(msgs, dir)
	if msgs[1].Parts != nil {
		t.Fatal("ExpandMedia modified its input")
	}
	if out[0].Parts != nil {
		t.Errorf("system message expanded: %+v", out[0].Parts)
	}
	parts := out[1].Parts
	var texts []string
	images := 0
	for _, p := range parts {
		if p.Image != nil {
			images++
			if p.Image.MediaType != "image/png" || p.Image.Data != base64.StdEncoding.EncodeToString(tinyPNG) {
				t.Errorf("image = %+v", p.Image)
			}
			continue
		}
		texts = append(texts, p.Text)
	}
	if images != 2 {
		t.Errorf("images = %d, want 2", images)
	}
	want := []string{
		"[image: " + png + "]",
		"[image: rel.png]",
		"[image: " + fake + " (not attached: unsupported format)]",
		"[image: " + big + " (not attached: too large)]",
		"[image: " + filepath.Join(dir, "missing.png") + " (not attached: unreadable)]",
		"what is this?",
	}
	if !slices.Equal(texts, want) {
		t.Errorf("texts =\n%q\nwant\n%q", texts, want)
	}
	if parts[1].Image == nil || parts[3].Image == nil {
		t.Errorf("each image should follow its placeholder: %+v", parts)
	}
}

func TestExpandMediaLimits(t *testing.T) {
	dir := t.TempDir()
	png := writeFile(t, dir, "a.png", tinyPNG)
	many := make([]string, MaxImagesPerMessage+1)
	for i := range many {
		many[i] = png
	}
	out := ExpandMedia([]Message{{Role: RoleUser, Media: many}}, "")
	images := 0
	for _, p := range out[0].Parts {
		if p.Image != nil {
			images++
		}
	}
	if images != MaxImagesPerMessage {
		t.Errorf("images = %d, want %d", images, MaxImagesPerMessage)
	}
	if last := out[0].Parts[len(out[0].Parts)-2].Text; !strings.Contains(last, "over 8 images") {
		t.Errorf("last placeholder = %q", last)
	}

	// The request budget goes to the newest message first.
	n := MaxRequestImageBytes/MaxImageBytes + 1
	big := writeFile(t, dir, "big.png", append(slices.Clone(tinyPNG), make([]byte, MaxImageBytes-len(tinyPNG))...))
	var msgs []Message
	for range n {
		msgs = append(msgs, Message{Role: RoleUser, Media: []string{big}})
	}
	out = ExpandMedia(msgs, "")
	if out[0].Parts[1].Image != nil || !strings.Contains(out[0].Parts[0].Text, "budget exhausted") {
		t.Errorf("oldest message should lose its image: %q", out[0].Parts[0].Text)
	}
	if out[n-1].Parts[1].Image == nil {
		t.Error("newest message should keep its image")
	}
}

func TestMessageJSONKeepsPathsOnly(t *testing.T) {
	m := Message{Role: RoleUser, Content: "hi", Media: []string{"/w/a.png"},
		Parts: []Part{{Image: &Image{MediaType: "image/png", Data: "QUJD"}}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"role":"user","content":"hi","media":["/w/a.png"]}` {
		t.Errorf("JSON = %s", b)
	}
}

func imageMessages() []Message {
	return []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "what?", Media: []string{"/w/a.png"}, Parts: []Part{
			{Text: "[image: /w/a.png]"},
			{Image: &Image{MediaType: "image/png", Data: "QUJD"}},
			{Text: "what?"},
		}},
		{Role: RoleAssistant, Content: "a cat"},
		{Role: RoleUser, Content: "", Parts: []Part{
			{Text: "[image: /w/b.jpg]"},
			{Image: &Image{MediaType: "image/jpeg", Data: "REVG"}},
			{Text: " "},
		}},
	}
}

func TestOpenAIImageParts(t *testing.T) {
	p := NewOpenAI(OpenAIConfig{Model: "gpt-x"})
	assertJSONEqual(t, p.buildRequest(ChatRequest{Messages: imageMessages()}), `{
		"model": "gpt-x",
		"messages": [
			{"role": "system", "content": "sys"},
			{"role": "user", "content": [
				{"type": "text", "text": "[image: /w/a.png]"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,QUJD"}},
				{"type": "text", "text": "what?"}
			]},
			{"role": "assistant", "content": "a cat"},
			{"role": "user", "content": [
				{"type": "text", "text": "[image: /w/b.jpg]"},
				{"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,REVG"}}
			]}
		]
	}`)
}

func TestAnthropicImageBlocks(t *testing.T) {
	p := NewAnthropic(AnthropicConfig{Model: "claude-x"})
	assertJSONEqual(t, p.buildRequest(ChatRequest{Messages: imageMessages()}), `{
		"model": "claude-x", "max_tokens": 8192, "system": "sys",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "[image: /w/a.png]"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "QUJD"}},
				{"type": "text", "text": "what?"}
			]},
			{"role": "assistant", "content": [{"type": "text", "text": "a cat"}]},
			{"role": "user", "content": [
				{"type": "text", "text": "[image: /w/b.jpg]"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "REVG"}}
			]}
		]
	}`)
}
