package weixin

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/providers"
)

var (
	testKey = []byte("0123456789abcdef")
	// testPNG starts with the PNG signature, so it is detected as one.
	testPNG = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 40)...)
)

// mediaMsg builds an inbound message with items.
func mediaMsg(id, from string, items ...map[string]any) map[string]any {
	list := make([]any, len(items))
	for i, it := range items {
		list[i] = it
	}
	return map[string]any{"message_id": id, "from_user_id": from, "to_user_id": "bot@im.bot",
		"message_type": 1, "context_token": "ctx", "item_list": list}
}

func imageWire(media map[string]any, hexKey string) map[string]any {
	ii := map[string]any{"media": media}
	if hexKey != "" {
		ii["aeskey"] = hexKey
	}
	return map[string]any{"type": itemImage, "image_item": ii}
}

func encrypted(t *testing.T, data, key []byte) string {
	t.Helper()
	out, err := encryptECB(data, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// serveCDN answers CDN downloads with files by encrypted_query_param
// (path /c2c/download) or by path (full URLs); anything else is a 404.
// It also creates the workspace, as the gateway does.
func (h *harness) serveCDN(t *testing.T, byParam, byPath map[string]string) {
	t.Helper()
	if err := os.MkdirAll(h.ws, 0o755); err != nil {
		t.Fatal(err)
	}
	h.f.setReply(func(c apiCall, _ int) (int, string, bool) {
		if c.Endpoint != "download" {
			return 0, "", false
		}
		if c.Path == "/c2c/download" {
			if body, ok := byParam[url.Values(c.Query).Get("encrypted_query_param")]; ok {
				return http.StatusOK, body, true
			}
		} else if body, ok := byPath[c.Path]; ok {
			return http.StatusOK, body, true
		}
		return http.StatusNotFound, "", true
	})
}

// checkSaved checks that rel is a 0600 file in MediaDir holding want.
func (h *harness) checkSaved(t *testing.T, rel string, want []byte) {
	t.Helper()
	if path.Dir(rel) != MediaDir {
		t.Fatalf("saved to %q, not in %s", rel, MediaDir)
	}
	full := filepath.Join(h.ws, filepath.FromSlash(rel))
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s: %d bytes, want %d", rel, len(got), len(want))
	}
	if fi, _ := os.Stat(full); fi.Mode().Perm() != 0o600 {
		t.Errorf("%s: mode %v", rel, fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Dir(full)); fi.Mode().Perm() != 0o700 {
		t.Errorf("%s: mode %v", MediaDir, fi.Mode())
	}
}

func TestInboundImageIsDownloaded(t *testing.T) {
	h := newHarness(t, nil)
	h.serveCDN(t, map[string]string{"img/param=1": encrypted(t, testPNG, testKey)}, nil)
	h.start(t)
	h.f.push([]map[string]any{mediaMsg("m1", alice,
		imageWire(map[string]any{"encrypt_query_param": "img/param=1"}, hex.EncodeToString(testKey)))}, "c1")

	in := h.inbound(t)
	if in.Content != "[image]" || len(in.Media) != 1 {
		t.Fatalf("inbound = %q, media %v", in.Content, in.Media)
	}
	if !strings.HasPrefix(path.Base(in.Media[0]), "image_") || path.Ext(in.Media[0]) != ".png" {
		t.Errorf("media path %q", in.Media[0])
	}
	h.checkSaved(t, in.Media[0], testPNG)
	dl := h.f.callsOf("download")
	if len(dl) != 1 || dl[0].Header.Get("Authorization") != "" {
		t.Errorf("CDN downloads %+v", dl)
	}
	// The agent inlines it for the model.
	msgs := providers.ExpandMedia([]providers.Message{{Role: providers.RoleUser, Content: in.Content, Media: in.Media}}, h.ws)
	if p := msgs[0].Parts; len(p) != 3 || p[1].Image == nil || p[1].Image.MediaType != "image/png" {
		t.Errorf("expanded parts %+v", p)
	}
	if strings.Contains(h.logs.String(), "img/param") {
		t.Errorf("the log has the download parameter:\n%s", h.logs)
	}
}

func TestInboundImageDownloadFails(t *testing.T) {
	h := newHarness(t, nil)
	h.serveCDN(t, nil, nil)
	h.start(t)
	h.f.push([]map[string]any{mediaMsg("m1", alice,
		imageWire(map[string]any{"encrypt_query_param": "gone"}, hex.EncodeToString(testKey)))}, "c1")
	in := h.inbound(t)
	if in.Content != "[image]" || in.Media != nil {
		t.Errorf("inbound = %q, media %v", in.Content, in.Media)
	}
	if !strings.Contains(h.logs.String(), "cannot download inbound media") {
		t.Errorf("no failure logged:\n%s", h.logs)
	}
}

func TestInboundFullURL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fullOK   bool
		wantGets []string
	}{
		{"preferred", true, []string{"/full/download"}},
		{"falls back", false, []string{"/full/download", "/c2c/download"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			byPath := map[string]string{}
			if tc.fullOK {
				byPath["/full/download"] = encrypted(t, testPNG, testKey)
			}
			h.serveCDN(t, map[string]string{"p": encrypted(t, testPNG, testKey)}, byPath)
			h.start(t)
			media := map[string]any{"full_url": h.f.srv.URL + "/full/download?k=v", "encrypt_query_param": "p",
				"aes_key": base64.StdEncoding.EncodeToString(testKey)}
			h.f.push([]map[string]any{mediaMsg("m1", alice, imageWire(media, ""))}, "c1")
			in := h.inbound(t)
			if len(in.Media) != 1 {
				t.Fatalf("media %v", in.Media)
			}
			h.checkSaved(t, in.Media[0], testPNG)
			var gets []string
			for _, c := range h.f.callsOf("download") {
				gets = append(gets, c.Path)
			}
			if !slices.Equal(gets, tc.wantGets) {
				t.Errorf("downloads %v, want %v", gets, tc.wantGets)
			}
		})
	}
}

func TestInboundFileVoiceVideo(t *testing.T) {
	h := newHarness(t, nil)
	pdf, voice, video := []byte("%PDF-1.4 report"), []byte("#!SILK_V3 voice"), []byte("....ftypmp42 video")
	hexKeyB64 := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(testKey)))
	h.serveCDN(t, map[string]string{
		"f": encrypted(t, pdf, testKey), "v": encrypted(t, voice, testKey), "mv": encrypted(t, video, testKey),
	}, nil)
	h.start(t)
	h.f.push([]map[string]any{mediaMsg("m1", alice,
		map[string]any{"type": itemFile, "file_item": map[string]any{"file_name": "../x/季度 report.pdf",
			"media": map[string]any{"encrypt_query_param": "f", "aes_key": hexKeyB64}}},
		map[string]any{"type": itemVoice, "voice_item": map[string]any{"text": "call me",
			"media": map[string]any{"encrypt_query_param": "v", "aes_key": hexKeyB64}}},
		map[string]any{"type": itemVideo, "video_item": map[string]any{
			"media": map[string]any{"encrypt_query_param": "mv", "aes_key": base64.StdEncoding.EncodeToString(testKey)}}},
	)}, "c1")

	in := h.inbound(t)
	if in.Content != "[file: ../x/季度 report.pdf]\n[voice] call me\n[video]" || len(in.Media) != 3 {
		t.Fatalf("inbound = %q, media %v", in.Content, in.Media)
	}
	if !strings.HasSuffix(in.Media[0], "_季度_report.pdf") || path.Ext(in.Media[1]) != ".silk" || path.Ext(in.Media[2]) != ".mp4" {
		t.Errorf("media %v", in.Media)
	}
	h.checkSaved(t, in.Media[0], pdf)
	h.checkSaved(t, in.Media[1], voice)
	h.checkSaved(t, in.Media[2], video)
}

func TestInboundMediaWithoutKey(t *testing.T) {
	h := newHarness(t, nil)
	h.serveCDN(t, map[string]string{"f": "plain"}, nil)
	h.start(t)
	h.f.push([]map[string]any{mediaMsg("m1", alice,
		map[string]any{"type": itemFile, "file_item": map[string]any{"file_name": "a.pdf",
			"media": map[string]any{"encrypt_query_param": "f"}}})}, "c1")
	in := h.inbound(t)
	if in.Content != "[file: a.pdf]" || in.Media != nil {
		t.Errorf("inbound = %q, media %v", in.Content, in.Media)
	}
	if n := len(h.f.callsOf("download")); n != 0 {
		t.Errorf("%d downloads", n)
	}
}

func TestInboundUnencryptedImage(t *testing.T) {
	h := newHarness(t, nil)
	h.serveCDN(t, map[string]string{"p": string(testPNG)}, nil)
	h.start(t)
	h.f.push([]map[string]any{mediaMsg("m1", alice, imageWire(map[string]any{"encrypt_query_param": "p"}, ""))}, "c1")
	in := h.inbound(t)
	if len(in.Media) != 1 {
		t.Fatalf("media %v", in.Media)
	}
	h.checkSaved(t, in.Media[0], testPNG)
}

func TestDeniedSenderMediaIsNotDownloaded(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AllowFrom = nil })
	h.serveCDN(t, map[string]string{"p": encrypted(t, testPNG, testKey)}, nil)
	h.start(t)
	h.f.push([]map[string]any{mediaMsg("m1", bob,
		imageWire(map[string]any{"encrypt_query_param": "p"}, hex.EncodeToString(testKey)))}, "c1")
	h.noInbound(t)
	if n := len(h.f.callsOf("download")); n != 0 {
		t.Errorf("%d downloads for a denied sender", n)
	}
	if _, err := os.Stat(filepath.Join(h.ws, MediaDir)); !os.IsNotExist(err) {
		t.Errorf("media dir: %v", err)
	}
}

func TestQuotedMediaFallback(t *testing.T) {
	quoting := func(text string) map[string]any {
		return map[string]any{"type": itemText, "text_item": map[string]any{"text": text},
			"ref_msg": map[string]any{"title": "pic", "message_item": imageWire(
				map[string]any{"encrypt_query_param": "ref"}, hex.EncodeToString(testKey))}}
	}
	t.Run("only quoted media", func(t *testing.T) {
		h := newHarness(t, nil)
		h.serveCDN(t, map[string]string{"ref": encrypted(t, testPNG, testKey)}, nil)
		h.start(t)
		h.f.push([]map[string]any{mediaMsg("m1", alice, quoting("what is this"))}, "c1")
		in := h.inbound(t)
		if in.Content != "what is this\n[image]" || len(in.Media) != 1 {
			t.Fatalf("inbound = %q, media %v", in.Content, in.Media)
		}
		h.checkSaved(t, in.Media[0], testPNG)
	})
	t.Run("top-level media failed", func(t *testing.T) {
		h := newHarness(t, nil)
		h.serveCDN(t, map[string]string{"ref": encrypted(t, testPNG, testKey)}, nil)
		h.start(t)
		h.f.push([]map[string]any{mediaMsg("m1", alice,
			imageWire(map[string]any{"encrypt_query_param": "gone"}, hex.EncodeToString(testKey)),
			quoting("and this"))}, "c1")
		in := h.inbound(t)
		if in.Content != "[image]\nand this" || in.Media != nil {
			t.Errorf("inbound = %q, media %v", in.Content, in.Media)
		}
		if n := len(h.f.callsOf("download")); n != 1 {
			t.Errorf("%d downloads, want only the top-level one", n)
		}
	})
}

func TestParseAESKey(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString
	for _, tc := range []struct {
		name, in string
		ok       bool
	}{
		{"raw", b64(testKey), true},
		{"raw unpadded", strings.TrimRight(b64(testKey), "="), true},
		{"hex", b64([]byte(hex.EncodeToString(testKey))), true},
		{"hex upper", b64([]byte(strings.ToUpper(hex.EncodeToString(testKey)))), true},
		{"short", b64([]byte("0123456789")), false},
		{"32 non-hex", b64(bytes.Repeat([]byte{'z'}, 32)), false},
		{"not base64", "!!!", false},
	} {
		k, err := parseAESKey(tc.in)
		if tc.ok && (err != nil || !bytes.Equal(k, testKey)) {
			t.Errorf("%s: %x, %v", tc.name, k, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: no error", tc.name)
		}
	}
}

func TestDecryptECB(t *testing.T) {
	for _, n := range []int{0, 1, 15, 16, 17, 1000} {
		data := bytes.Repeat([]byte{'a'}, n)
		out, _ := encryptECB(data, testKey)
		got, err := decryptECB(out, testKey)
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%d bytes: roundtrip %v", n, err)
		}
	}
	out, _ := encryptECB([]byte("hello"), testKey)
	if _, err := decryptECB(out, []byte("fedcba9876543210")); err == nil {
		t.Error("wrong key: no padding error")
	}
	if _, err := decryptECB(out[:10], testKey); err == nil {
		t.Error("partial block: no error")
	}
}

func TestSafeFileName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":                      "report.pdf",
		"../../etc/passwd":                "passwd",
		`..\..\evil.exe`:                  "evil.exe",
		".hidden":                         "hidden",
		"a b/c;d.txt":                     "c_d.txt",
		"..":                              "",
		strings.Repeat("x", 200) + ".pdf": strings.Repeat("x", 96) + ".pdf",
	} {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
