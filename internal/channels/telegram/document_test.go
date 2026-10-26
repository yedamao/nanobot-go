package telegram

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// pdfFixture stands in for a document.
var pdfFixture = []byte("%PDF-1.4\n%fixture\n")

func documentDM(id int64, caption string) update {
	u := dm(id, alice, "")
	u.Message.Caption = caption
	u.Message.Document = &document{FileID: "doc", FileUniqueID: "AgADdoc", FileName: "report.pdf",
		MimeType: "application/pdf", FileSize: int64(len(pdfFixture))}
	return u
}

const documentPath = "media/telegram/AgADdoc.pdf"

// checkSavedDocument checks that rel is in the workspace with the
// fixture bytes and mode 0600.
func checkSavedDocument(t *testing.T, h *harness, rel string) {
	t.Helper()
	p := filepath.Join(h.workspace, filepath.FromSlash(rel))
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, pdfFixture) {
		t.Errorf("saved %d bytes that differ from the fixture", len(data))
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestDocument(t *testing.T) {
	h := newHarness(t, nil)
	tr := &fakeTranscriber{text: "never"}
	h.ch.transcriber = tr
	h.f.addFile("doc", pdfFixture)
	h.start(t)
	h.push(documentDM(1, ""))
	in := h.inbound(t)
	if in.Content != "[file: "+documentPath+"]" || !reflect.DeepEqual(in.Media, []string{documentPath}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	checkSavedDocument(t, h, documentPath)
	calls := h.f.callsOf("getFile")
	if len(calls) != 1 || calls[0].Body["file_id"] != "doc" {
		t.Errorf("getFile calls = %+v", calls)
	}

	// The caption comes first, then the path.
	h.push(documentDM(2, "summarise this"))
	if in := h.inbound(t); in.Content != "summarise this\n[file: "+documentPath+"]" || len(in.Media) != 1 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	// Documents are not transcribed, even audio ones.
	u := dm(3, alice, "")
	u.Message.Document = &document{FileID: "doc", FileUniqueID: "AgADdocogg", MimeType: "audio/ogg"}
	h.push(u)
	if in := h.inbound(t); !reflect.DeepEqual(in.Media, []string{"media/telegram/AgADdocogg.ogg"}) {
		t.Errorf("media = %v", in.Media)
	}
	if len(tr.names) != 0 {
		t.Errorf("transcriber called with %v", tr.names)
	}
}

func TestDocumentExtension(t *testing.T) {
	for _, tc := range []struct {
		name, mime, fileName, want string
	}{
		{"known MIME wins", "image/png", "photo.jpeg", ".png"},
		{"video MIME", "video/quicktime", "", ".mov"},
		{"by file name", "application/pdf", "report.pdf", ".pdf"},
		{"all suffixes", "application/gzip", "backup.tar.gz", ".tar.gz"},
		{"no MIME or name", "", "", ""},
		{"no suffix", "", "README", ""},
		{"dot file", "", ".bashrc", ""},
		{"trailing dot", "", "notes.", ""},
		{"path in the name", "", "../../etc/passwd.sh", ".sh"},
		{"backslashes", "", `..\..\evil.bat`, ".bat"},
		{"only trailing safe suffixes", "", "My Report v1.2 final.docx", ".docx"},
		{"unsafe suffix", "", "x.p/df", ""},
		{"too long", "", "x." + strings.Repeat("a", maxFileExtLen), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := clip{kind: "file", mimeType: tc.mime, fileName: tc.fileName}
			if got := k.ext(); got != tc.want {
				t.Errorf("ext() = %q, want %q", got, tc.want)
			}
		})
	}

	// End to end: the extension comes from the file name.
	h := newHarness(t, nil)
	h.f.addFile("doc", pdfFixture)
	h.start(t)
	u := dm(1, alice, "")
	u.Message.Document = &document{FileID: "doc", FileUniqueID: "AgADtgz", FileName: "../backup.tar.gz", MimeType: "application/x-gtar"}
	h.push(u)
	const want = "media/telegram/AgADtgz.tar.gz"
	if in := h.inbound(t); in.Content != "[file: "+want+"]" || !reflect.DeepEqual(in.Media, []string{want}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	checkSavedDocument(t, h, want)
}

func TestDocumentDownloadFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.start(t) // no file: getFile fails
	h.push(documentDM(1, "caption"))
	if in := h.inbound(t); in.Content != "caption" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v, want the caption alone", in.Content, in.Media)
	}
	// A document alone is dropped.
	h.push(documentDM(2, ""), dm(3, alice, "next"))
	if in := h.inbound(t); in.Content != "next" {
		t.Errorf("got %q after a bare document, want the text after it", in.Content)
	}
	if !strings.Contains(h.logs.String(), "could not download a file message") {
		t.Errorf("no warning logged:\n%s", h.logs)
	}
}

func TestDocumentTooLarge(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("doc", pdfFixture)
	h.start(t)
	u := documentDM(1, "big")
	u.Message.Document.FileSize = maxClipBytes + 1
	h.push(u)
	if in := h.inbound(t); in.Content != "big" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	u = documentDM(2, "")
	u.Message.Document.FileSize = maxClipBytes + 1
	h.push(u, dm(3, alice, "next"))
	if in := h.inbound(t); in.Content != "next" {
		t.Errorf("got %q after a large bare document, want the text after it", in.Content)
	}
	if n := len(h.f.callsOf("getFile")); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
}

func TestReplyToDocument(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("doc", pdfFixture)
	h.f.addFile("large", jpegFixture)
	h.start(t)
	// The replied-to document comes before the message's own photo.
	u := photoDM(1, "and this?", photoSizes)
	u.Message.ReplyToMessage = documentDM(9, "").Message
	h.push(u)
	in := h.inbound(t)
	if in.Content != "[Reply to: [file: "+documentPath+"]]\nand this?" {
		t.Errorf("content = %q", in.Content)
	}
	if want := []string{documentPath, "media/telegram/AgADuq_Large.jpg"}; !reflect.DeepEqual(in.Media, want) {
		t.Errorf("media = %v, want %v", in.Media, want)
	}
	// A captioned one is quoted by its caption.
	u = dm(2, alice, "summarise")
	u.Message.ReplyToMessage = documentDM(8, "from Bob").Message
	h.push(u)
	if in := h.inbound(t); in.Content != "[Reply to @alice: from Bob]\nsummarise" || !reflect.DeepEqual(in.Media, []string{documentPath}) {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
}

func TestDocumentCaptionCommand(t *testing.T) {
	h := newHarness(t, nil)
	h.f.addFile("doc", pdfFixture)
	h.start(t)
	h.push(documentDM(1, "/new"))
	if in := h.inbound(t); in.Content != "/new" || len(in.Media) != 0 {
		t.Errorf("got content %q media %v", in.Content, in.Media)
	}
	if n := len(h.f.callsOf("getFile")); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
}
