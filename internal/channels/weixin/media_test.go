package weixin

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yedamao/nanobot-go/internal/bus"
	"github.com/yedamao/nanobot-go/internal/channels"
	"github.com/yedamao/nanobot-go/internal/security"
)

// writeFile writes data to rel in the workspace and returns its path.
func (h *harness) writeFile(t *testing.T, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(h.ws, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sendMedia(h *harness, content string, media ...string) error {
	return h.ch.Send(context.Background(), bus.OutboundMessage{Channel: Name, ChatID: alice, Content: content, Media: media})
}

// sentItem returns the single item of a sendmessage.
func sentItem(t *testing.T, c apiCall) map[string]any {
	t.Helper()
	msg, _ := c.Body["msg"].(map[string]any)
	items, _ := msg["item_list"].([]any)
	if len(items) != 1 {
		t.Fatalf("item_list = %v", msg["item_list"])
	}
	return items[0].(map[string]any)
}

// itemKinds lists the item keys of sendmessage calls ("text_item", ...).
func itemKinds(t *testing.T, calls []apiCall) string {
	t.Helper()
	var kinds []string
	for _, c := range calls {
		for k := range sentItem(t, c) {
			if strings.HasSuffix(k, "_item") {
				kinds = append(kinds, k)
			}
		}
	}
	return strings.Join(kinds, ",")
}

// mustDecrypt decrypts an upload with key.
func mustDecrypt(t *testing.T, data, key []byte) []byte {
	t.Helper()
	out, err := decryptECB(data, key)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEncryptECB(t *testing.T) {
	// FIPS-197 appendix C.1.
	key, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	plain, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	out, err := encryptECB(plain, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(out[:16]); got != "69c4e0d86a7b0430d8cdb78070b4c55a" || len(out) != 32 {
		t.Errorf("ciphertext = %x", out)
	}
	for _, n := range []int{0, 1, 15, 16, 17, 1000} {
		data := bytes.Repeat([]byte{'a'}, n)
		out, err := encryptECB(data, key)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != paddedSize(n) {
			t.Errorf("%d bytes: ciphertext %d, paddedSize %d", n, len(out), paddedSize(n))
		}
		if !bytes.Equal(mustDecrypt(t, out, key), data) {
			t.Errorf("%d bytes: roundtrip differs", n)
		}
	}
}

func TestSendMediaThenText(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	h.f.waitFor("sendtyping", 1)
	png := bytes.Repeat([]byte("PNG"), 100) // 300 bytes
	h.writeFile(t, "out/chart.png", png)
	pdf := []byte("%PDF-1.4 report")
	pdfPath := h.writeFile(t, "report.pdf", pdf)
	if err := sendMedia(h, "done", "out/chart.png", pdfPath); err != nil {
		t.Fatal(err)
	}
	calls := h.f.callsOf("sendmessage")
	if got := itemKinds(t, calls); got != "image_item,file_item,text_item" {
		t.Fatalf("sent %s", got)
	}
	texts, toks, ids := sentTexts(calls)
	if texts[2] != "done" || toks[0] != "ctx-1" || toks[1] != "ctx-1" || ids[0] == ids[1] || ids[1] == ids[2] {
		t.Errorf("texts %q, tokens %q, ids %q", texts, toks, ids)
	}

	ups := h.f.callsOf("getuploadurl")
	cdn := h.f.callsOf("upload")
	if len(ups) != 2 || len(cdn) != 2 {
		t.Fatalf("%d getuploadurl, %d CDN uploads", len(ups), len(cdn))
	}
	// The image: getuploadurl describes it, the CDN gets it encrypted with
	// the key, and sendmessage references the upload.
	up := ups[0].Body
	sum := md5.Sum(png)
	if up["media_type"] != float64(uploadMediaImage) || up["to_user_id"] != alice || up["rawsize"] != float64(len(png)) ||
		up["rawfilemd5"] != hex.EncodeToString(sum[:]) || up["filesize"] != float64(paddedSize(len(png))) ||
		up["no_need_thumb"] != true || up["base_info"] == nil {
		t.Errorf("getuploadurl = %v", up)
	}
	keyHex, _ := up["aeskey"].(string)
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 16 {
		t.Fatalf("aeskey %q", keyHex)
	}
	fileKey, _ := up["filekey"].(string)
	if len(fileKey) != 32 {
		t.Errorf("filekey %q", fileKey)
	}
	if cdn[0].Path != "/c2c/upload" || url.Values(cdn[0].Query).Get("encrypted_query_param") != "up/param=1" || url.Values(cdn[0].Query).Get("filekey") != fileKey {
		t.Errorf("CDN upload to %s?%v", cdn[0].Path, cdn[0].Query)
	}
	if cdn[0].Header.Get("Authorization") != "" {
		t.Error("the bot token went to the CDN")
	}
	if len(cdn[0].Raw) != paddedSize(len(png)) || !bytes.Equal(mustDecrypt(t, cdn[0].Raw, key), png) {
		t.Errorf("CDN body: %d bytes", len(cdn[0].Raw))
	}
	img := sentItem(t, calls[0])
	ii, _ := img["image_item"].(map[string]any)
	media, _ := ii["media"].(map[string]any)
	if img["type"] != float64(itemImage) || ii["mid_size"] != float64(paddedSize(len(png))) ||
		media["encrypt_query_param"] != "dl-param-1" || media["encrypt_type"] != float64(1) ||
		media["aes_key"] != base64.StdEncoding.EncodeToString([]byte(keyHex)) {
		t.Errorf("image item = %v", img)
	}

	// The PDF is a file item with its name and size.
	if ups[1].Body["media_type"] != float64(uploadMediaFile) {
		t.Errorf("getuploadurl media_type = %v", ups[1].Body["media_type"])
	}
	file := sentItem(t, calls[1])
	fi, _ := file["file_item"].(map[string]any)
	fm, _ := fi["media"].(map[string]any)
	if file["type"] != float64(itemFile) || fi["file_name"] != "report.pdf" || fi["len"] != strconv.Itoa(len(pdf)) ||
		fm["encrypt_query_param"] != "dl-param-2" {
		t.Errorf("file item = %v", file)
	}
	// Sending clears the typing indicator.
	if typing := h.f.waitFor("sendtyping", 2); typing[len(typing)-1].Body["status"] != float64(typingStatusCancel) {
		t.Error("typing not cancelled")
	}
}

func TestSendMediaItemTypes(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	h.writeFile(t, "clip.MP4", []byte("video"))
	h.writeFile(t, "note.mp3", []byte("voice"))
	if err := sendMedia(h, "", "clip.MP4", "note.mp3"); err != nil {
		t.Fatal(err)
	}
	calls := h.f.callsOf("sendmessage")
	if got := itemKinds(t, calls); got != "video_item,voice_item" {
		t.Fatalf("sent %s (media only, no text)", got)
	}
	ups := h.f.callsOf("getuploadurl")
	if ups[0].Body["media_type"] != float64(uploadMediaVideo) || ups[1].Body["media_type"] != float64(uploadMediaVoice) {
		t.Errorf("media types %v, %v", ups[0].Body["media_type"], ups[1].Body["media_type"])
	}
	vi := sentItem(t, calls[0])["video_item"].(map[string]any)
	if vi["video_size"] != float64(paddedSize(5)) || vi["media"] == nil {
		t.Errorf("video item = %v", vi)
	}
}

func TestUploadFullURLIsPreferred(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	h.f.setReply(func(c apiCall, n int) (int, string, bool) {
		return 200, `{"ret":0,"upload_param":"p","upload_full_url":"` + h.f.srv.URL + `/full/upload?k=v"}`, c.Endpoint == "getuploadurl"
	})
	h.writeFile(t, "a.png", []byte("img"))
	if err := sendMedia(h, "", "a.png"); err != nil {
		t.Fatal(err)
	}
	cdn := h.f.callsOf("upload")
	if len(cdn) != 1 || cdn[0].Path != "/full/upload" || url.Values(cdn[0].Query).Get("k") != "v" {
		t.Fatalf("CDN uploads %v", cdn)
	}
}

func TestUnsendableFilesFallBackToNotice(t *testing.T) {
	cases := map[string]replyFunc{
		"missing file": nil,
		"CDN refuses": func(c apiCall, n int) (int, string, bool) {
			return 400, `bad`, c.Endpoint == "upload"
		},
		"no download param": func(c apiCall, n int) (int, string, bool) {
			return 200, ``, c.Endpoint == "upload"
		},
		"getuploadurl refuses": func(c apiCall, n int) (int, string, bool) {
			return 200, `{"ret":-1,"errmsg":"bad file"}`, c.Endpoint == "getuploadurl"
		},
		"no upload URL": func(c apiCall, n int) (int, string, bool) {
			return 200, `{"ret":0}`, c.Endpoint == "getuploadurl"
		},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.start(t)
			h.greet(t, "m1", "ctx-1")
			if reply != nil {
				h.writeFile(t, "pics/x.png", []byte("img"))
				h.f.setReply(reply)
			}
			if err := sendMedia(h, "the text", "pics/x.png"); err != nil {
				t.Fatal(err)
			}
			texts, _, _ := sentTexts(h.f.callsOf("sendmessage"))
			if strings.Join(texts, "|") != "[Failed to send: x.png]|the text" {
				t.Errorf("sent %q", texts)
			}
		})
	}
}

func TestMediaRetryResumesWithSameUpload(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	h.f.setReply(func(c apiCall, n int) (int, string, bool) {
		return 503, `busy`, c.Endpoint == "upload" && n == 1
	})
	h.writeFile(t, "a.png", []byte("img"))
	err := sendMedia(h, "text", "a.png")
	if err == nil || channels.IsPermanent(err) {
		t.Fatalf("first attempt = %v, want a retryable error", err)
	}
	if n := len(h.f.callsOf("sendmessage")); n != 0 {
		t.Fatalf("%d messages sent after a failed upload", n)
	}
	if err := sendMedia(h, "text", "a.png"); err != nil {
		t.Fatal(err)
	}
	if got := itemKinds(t, h.f.callsOf("sendmessage")); got != "image_item,text_item" {
		t.Errorf("sent %s", got)
	}
	ups := h.f.callsOf("getuploadurl")
	if len(ups) != 2 || ups[0].Body["aeskey"] != ups[1].Body["aeskey"] || ups[0].Body["filekey"] != ups[1].Body["filekey"] {
		t.Errorf("the retry uploaded with another key: %d getuploadurl", len(ups))
	}
}

func TestMediaCountsAgainstContextBudget(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	for i := range contextMessageBudget - 1 {
		if err := send(t, h, "msg "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	h.writeFile(t, "a.png", []byte("a"))
	h.writeFile(t, "b.png", []byte("b"))
	// One message left: the first file goes, the rest is held.
	if err := sendMedia(h, "caption", "a.png", "b.png"); err != nil {
		t.Fatalf("Send = %v, want nil (held)", err)
	}
	calls := h.f.callsOf("sendmessage")
	if len(calls) != contextMessageBudget || itemKinds(t, calls[len(calls)-1:]) != "image_item" {
		t.Fatalf("%d sent, last %s", len(calls), itemKinds(t, calls[len(calls)-1:]))
	}
	h.greet(t, "m2", "ctx-2")
	rest := h.f.waitFor("sendmessage", contextMessageBudget+2)[contextMessageBudget:]
	texts, toks, _ := sentTexts(rest)
	if itemKinds(t, rest) != "image_item,text_item" || texts[1] != "caption" || toks[0] != "ctx-2" {
		t.Errorf("held parts sent: %s %q %q", itemKinds(t, rest), texts, toks)
	}
	if n := len(h.f.callsOf("upload")); n != 2 {
		t.Errorf("%d CDN uploads, want 2", n)
	}
}

func TestTooManyFilesAreCapped(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	h.writeFile(t, "a.txt", []byte("a"))
	media := make([]string, maxOutboundFiles+2)
	for i := range media {
		media[i] = "a.txt"
	}
	sendMedia(h, "", media...)
	// The budget allows 8 messages; the other two are held.
	if n := len(h.f.callsOf("upload")); n != contextMessageBudget {
		t.Fatalf("%d uploads", n)
	}
	h.greet(t, "m2", "ctx-2")
	h.f.waitFor("upload", maxOutboundFiles)
	h.greet(t, "m3", "ctx-3")
	if n := len(h.f.callsOf("upload")); n != maxOutboundFiles {
		t.Errorf("%d uploads, want %d", n, maxOutboundFiles)
	}
}

func TestSendRemoteMediaDownloadsThenUploads(t *testing.T) {
	h := newHarness(t, nil)
	png := bytes.Repeat([]byte("PNG"), 50)
	var gets atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		switch r.URL.Path {
		case "/img/cat.png":
			w.Write(png)
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	// The fixture is on loopback: whitelist it as the public host it
	// stands in for.
	h.ch.ssrf = &security.Policy{Whitelist: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	if err := sendMedia(h, "look", remote.URL+"/img/cat.png?sig=1", remote.URL+"/missing.pdf"); err != nil {
		t.Fatal(err)
	}
	calls := h.f.callsOf("sendmessage")
	if got := itemKinds(t, calls); got != "image_item,text_item,text_item" {
		t.Fatalf("sent %s", got)
	}
	if texts, _, _ := sentTexts(calls); texts[1] != "[Failed to send: missing.pdf]" || texts[2] != "look" {
		t.Errorf("texts %q", texts)
	}
	ups, cdn := h.f.callsOf("getuploadurl"), h.f.callsOf("upload")
	if len(ups) != 1 || len(cdn) != 1 || ups[0].Body["media_type"] != float64(uploadMediaImage) || ups[0].Body["rawsize"] != float64(len(png)) {
		t.Fatalf("getuploadurl %v, %d CDN uploads", ups, len(cdn))
	}
	key, _ := hex.DecodeString(ups[0].Body["aeskey"].(string))
	if !bytes.Equal(mustDecrypt(t, cdn[0].Raw, key), png) {
		t.Error("the CDN did not get the downloaded file")
	}
	if gets.Load() != 2 {
		t.Errorf("%d downloads", gets.Load())
	}
}

func TestSendRemoteMediaBlockedBySSRF(t *testing.T) {
	h := newHarness(t, nil)
	var gets atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.Write([]byte("secret"))
	}))
	defer remote.Close()
	h.start(t)
	h.greet(t, "m1", "ctx-1")
	// The default policy (no whitelist): loopback, private and metadata
	// addresses are never fetched.
	if err := sendMedia(h, "x", remote.URL+"/a.png", "http://10.0.0.1/b.png", "http://169.254.169.254/latest/c.png"); err != nil {
		t.Fatal(err)
	}
	texts, _, _ := sentTexts(h.f.callsOf("sendmessage"))
	if strings.Join(texts, "|") != "[Failed to send: a.png]|[Failed to send: b.png]|[Failed to send: c.png]|x" {
		t.Errorf("sent %q", texts)
	}
	if gets.Load() != 0 || len(h.f.callsOf("getuploadurl")) != 0 {
		t.Errorf("%d downloads, %d upload requests", gets.Load(), len(h.f.callsOf("getuploadurl")))
	}
}
