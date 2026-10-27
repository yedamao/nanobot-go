package mcp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func newTestStore(t *testing.T) *OAuthStore {
	t.Helper()
	return NewOAuthStore(filepath.Join(t.TempDir(), "auth", "mcp.json"), nil)
}

func saveTestAuth(t *testing.T, b *oauthBinding, access, refresh string) {
	t.Helper()
	meta := &storedMetadata{Issuer: "https://as.example", TokenEndpoint: "https://as.example/token"}
	ok, err := b.saveAuthorization(meta, "https://as.example/", &storedClient{ClientID: "cid", TokenEndpointAuthMethod: "none"},
		&oauth2.Token{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
	if err != nil || !ok {
		t.Fatalf("saveAuthorization = %v, %v", ok, err)
	}
}

func TestServerFingerprint(t *testing.T) {
	// SHA-256 of the trimmed URL, as nanobot computes it.
	if got, want := serverFingerprint("  https://h/mcp \n"), serverFingerprint("https://h/mcp"); got != want {
		t.Errorf("fingerprint not trimmed: %s != %s", got, want)
	}
	if got := serverFingerprint("https://h/mcp"); got != "d94576a5b196fdcce74f8dc1aec844fc67e8e76d0ce2f8a0828b377eb5be1db1" {
		t.Errorf("fingerprint = %q, want sha256 hex of the URL", got)
	}
	if serverFingerprint("https://h/mcp") == serverFingerprint("https://h/other") {
		t.Error("different URLs share a fingerprint")
	}
}

func TestOAuthStoreRoundTripAndShape(t *testing.T) {
	st := newTestStore(t)
	if st.HasCredentials("linear", "https://h/mcp") {
		t.Fatal("empty store has credentials")
	}
	saveTestAuth(t, st.bind("linear", "https://h/mcp"), "at", "rt")
	if !st.HasCredentials("linear", "https://h/mcp") {
		t.Fatal("saved credentials not found")
	}
	snap, err := st.bind("linear", "https://h/mcp").snapshot()
	if err != nil {
		t.Fatal(err)
	}
	tok := snap.token()
	if tok == nil || tok.AccessToken != "at" || tok.RefreshToken != "rt" || time.Until(tok.Expiry) < 59*time.Minute {
		t.Errorf("token = %+v", tok)
	}
	if snap.tokenIssuer != "https://as.example" || snap.oauthIssuer != "https://as.example" {
		t.Errorf("issuers = %q, %q (want normalized)", snap.tokenIssuer, snap.oauthIssuer)
	}

	data, err := os.ReadFile(st.path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["version"] != float64(1) {
		t.Errorf("version = %v", raw["version"])
	}
	entry := raw["servers"].(map[string]any)["linear"].(map[string]any)
	for _, k := range []string{"server_fingerprint", "write_lease", "tokens", "expires_at", "token_issuer", "client_info", "oauth_metadata", "oauth_issuer"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("entry lacks %q: %v", k, entry)
		}
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(st.path); fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Dir(st.path)); fi.Mode().Perm() != 0o700 {
			t.Errorf("dir mode = %v, want 0700", fi.Mode().Perm())
		}
	}
}

func TestOAuthStoreIsolation(t *testing.T) {
	st := newTestStore(t)
	saveTestAuth(t, st.bind("a", "https://h/mcp"), "at-a", "rt-a")
	if st.HasCredentials("b", "https://h/mcp") {
		t.Error("another name sees a's tokens")
	}
	if st.HasCredentials("a", "https://other/mcp") {
		t.Error("a re-pointed server sees the old URL's tokens")
	}
	// Re-pointing name a replaces its entry rather than sharing it.
	saveTestAuth(t, st.bind("a", "https://other/mcp"), "at-o", "rt-o")
	if st.HasCredentials("a", "https://h/mcp") {
		t.Error("old URL's tokens survive re-pointing")
	}
}

func TestOAuthStoreDeleteRotatesGeneration(t *testing.T) {
	st := newTestStore(t)
	b := st.bind("a", "https://h/mcp")
	saveTestAuth(t, b, "at", "rt")
	// A flow bound before the deletion.
	stale := st.bind("a", "https://h/mcp")

	removed, err := st.DeleteCredentials("a")
	if err != nil || !removed {
		t.Fatalf("DeleteCredentials = %v, %v", removed, err)
	}
	if st.HasCredentials("a", "https://h/mcp") {
		t.Fatal("credentials survive deletion")
	}
	gen1 := st.readUnlocked().Generations["a"]
	if gen1 == "" {
		t.Fatal("no generation after deletion")
	}
	// Writes and claims from before the deletion are ignored.
	if ok, _ := b.saveAuthorization(&storedMetadata{Issuer: "x", TokenEndpoint: "x/token"}, "x", &storedClient{ClientID: "c"}, &oauth2.Token{AccessToken: "late"}); ok {
		t.Error("a stale binding wrote credentials back")
	}
	if err := stale.prepareRedirectURI("http://127.0.0.1/auth/mcp/callback", false); !errors.Is(err, ErrAuthorizationRequired) {
		t.Errorf("stale claim = %v, want ErrAuthorizationRequired", err)
	}
	if st.HasCredentials("a", "https://h/mcp") {
		t.Error("credentials resurrected")
	}
	// Deleting again (no entry) still rotates.
	removed, _ = st.DeleteCredentials("a")
	if removed || st.readUnlocked().Generations["a"] == gen1 {
		t.Errorf("second delete: removed=%v, generation not rotated", removed)
	}
	// A fresh binding works again.
	saveTestAuth(t, st.bind("a", "https://h/mcp"), "at2", "rt2")
	if !st.HasCredentials("a", "https://h/mcp") {
		t.Error("cannot save after deletion")
	}
}

func TestOAuthStoreClaimLease(t *testing.T) {
	st := newTestStore(t)
	older := st.bind("a", "https://h/mcp")
	saveTestAuth(t, older, "at", "rt")
	// A newer browser flow claims the entry; the older binding's writes
	// are then ignored.
	newer := st.bind("a", "https://h/mcp")
	if err := newer.prepareRedirectURI("https://gw/auth/mcp/callback", false); err != nil {
		t.Fatal(err)
	}
	if ok, _ := older.clearTokens(); ok {
		t.Error("an older binding wrote after a newer claim")
	}
	snap, _ := newer.snapshot()
	if snap.client != nil {
		t.Error("a changed redirect URI kept the client registered for the old one")
	}
	if snap.token() == nil {
		t.Error("claim without reset dropped the tokens")
	}
	if snap.redirectURI != "https://gw/auth/mcp/callback" {
		t.Errorf("redirect = %q", snap.redirectURI)
	}
	// Same redirect, reset: tokens and client go.
	saveTestAuth(t, newer, "at2", "rt2")
	again := st.bind("a", "https://h/mcp")
	if err := again.prepareRedirectURI("https://gw/auth/mcp/callback", true); err != nil {
		t.Fatal(err)
	}
	snap, _ = again.snapshot()
	if snap.token() != nil || snap.client != nil {
		t.Error("reset kept tokens or client")
	}
}

func TestOAuthStoreFinishRefreshNeedsLease(t *testing.T) {
	st := newTestStore(t)
	b := st.bind("a", "https://h/mcp")
	saveTestAuth(t, b, "at", "rt")
	lease := refreshLease{"at", "rt", "https://as.example"}
	// Another process refreshed meanwhile: the result is not committed.
	saveTestAuth(t, b, "at-other", "rt-other")
	if ok, _ := b.finishRefresh(lease, &oauth2.Token{AccessToken: "mine"}, false); ok {
		t.Error("committed over another refresh")
	}
	lease = refreshLease{"at-other", "rt-other", "https://as.example"}
	if ok, _ := b.finishRefresh(lease, &oauth2.Token{AccessToken: "new", RefreshToken: "rt-new"}, false); !ok {
		t.Fatal("refresh not committed")
	}
	snap, _ := b.snapshot()
	if snap.token().AccessToken != "new" || snap.tokenIssuer != "https://as.example" {
		t.Errorf("after refresh: %+v issuer %q", snap.token(), snap.tokenIssuer)
	}
	// A refused refresh clears tokens and, with clearClient, the client.
	lease = refreshLease{"new", "rt-new", "https://as.example"}
	if ok, _ := b.finishRefresh(lease, nil, true); !ok {
		t.Fatal("clear not committed")
	}
	snap, _ = b.snapshot()
	if snap.token() != nil || snap.client != nil {
		t.Error("tokens or client kept")
	}
}

func TestOAuthStoreToleratesDamage(t *testing.T) {
	st := newTestStore(t)
	if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.path, []byte(`{"servers":{"bad":"x","ok":{"server_fingerprint":"`+serverFingerprint("https://h")+`","tokens":{"access_token":"t"}}},"generations":{"g":7}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !st.HasCredentials("ok", "https://h") {
		t.Error("good entry dropped")
	}
	if err := os.WriteFile(st.path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st.HasCredentials("ok", "https://h") {
		t.Error("damaged file read as credentials")
	}
}
