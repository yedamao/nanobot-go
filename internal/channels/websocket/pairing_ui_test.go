package websocket

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yedamao/nanobot-go/internal/pairing"
)

func TestPairingUIApproveDenyRevoke(t *testing.T) {
	dir := t.TempDir()
	store := pairing.NewStore(filepath.Join(dir, "pairing.json"))
	code, err := store.Code("telegram", "42")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, true, Options{Host: "127.0.0.1", Pairing: store})
	client := h.srv.Client()

	resp, err := client.Get(h.srv.URL + PairingPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	var view PairingView
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Pending) != 1 || view.Pending[0].Code != code || view.Pending[0].SenderID != "42" {
		t.Fatalf("pending = %+v", view.Pending)
	}
	if strings.Contains(string(body), dir) {
		t.Error("leaked absolute path")
	}

	norm := strings.ToLower(strings.ReplaceAll(code, "-", ""))
	resp, err = client.Post(h.srv.URL+PairingPath+"/approve", "application/json", strings.NewReader(`{"code":"`+norm+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d %s", resp.StatusCode, body)
	}

	resp, err = client.Get(h.srv.URL + PairingPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	json.Unmarshal(body, &view)
	if len(view.Pending) != 0 || len(view.Approved["telegram"]) != 1 {
		t.Fatalf("after approve = %+v", view)
	}

	resp, err = client.Post(h.srv.URL+PairingPath+"/revoke", "application/json", strings.NewReader(`{"channel":"telegram","sender_id":"42"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d", resp.StatusCode)
	}

	resp, err = client.Post(h.srv.URL+PairingPath+"/deny", "application/json", strings.NewReader(`{"code":"ZZZZ-ZZZZ"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deny missing = %d", resp.StatusCode)
	}

	code2, err := store.Code("telegram", "99")
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Post(h.srv.URL+PairingPath+"/deny", "application/json", strings.NewReader(`{"code":"`+code2+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("deny = %d", resp.StatusCode)
	}
}
