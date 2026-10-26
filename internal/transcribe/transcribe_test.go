package transcribe

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTranscribe(t *testing.T) {
	var got struct {
		path, auth, model, filename, ct string
		data                            []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		got.model = r.FormValue("model")
		f, fh, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		got.filename, got.ct = fh.Filename, fh.Header.Get("Content-Type")
		got.data, _ = io.ReadAll(f)
		io.WriteString(w, `{"text":"  hello there \n"}`)
	}))
	defer srv.Close()
	o := &OpenAI{APIKey: "sk-test", APIBase: srv.URL + "/v1/", HTTPClient: srv.Client()}
	text, err := o.Transcribe(t.Context(), "media/telegram/abc.ogg", []byte("OggS..."))
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello there" {
		t.Errorf("text = %q", text)
	}
	if got.path != "/v1/audio/transcriptions" || got.auth != "Bearer sk-test" || got.model != DefaultModel ||
		got.filename != "abc.ogg" || got.ct != "audio/ogg" || string(got.data) != "OggS..." {
		t.Errorf("request = %+v", got)
	}
}

func TestTranscribeErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad key"}}`)
	}))
	defer srv.Close()
	o := &OpenAI{APIKey: "sk-secret", APIBase: srv.URL + "/audio/transcriptions", HTTPClient: srv.Client()}
	_, err := o.Transcribe(t.Context(), "a.mp3", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "sk-secret") {
		t.Errorf("err = %v", err)
	}
	if _, err := (&OpenAI{}).Transcribe(t.Context(), "a.ogg", nil); err == nil {
		t.Error("no error without an API key")
	}
}

func TestEndpoint(t *testing.T) {
	for base, want := range map[string]string{
		"":                                  "https://api.openai.com/v1/audio/transcriptions",
		"https://api.groq.com/openai/v1/":   "https://api.groq.com/openai/v1/audio/transcriptions",
		"https://x/v1/audio/transcriptions": "https://x/v1/audio/transcriptions",
	} {
		if got := (&OpenAI{APIBase: base}).endpoint(); got != want {
			t.Errorf("endpoint(%q) = %q, want %q", base, got, want)
		}
	}
}
