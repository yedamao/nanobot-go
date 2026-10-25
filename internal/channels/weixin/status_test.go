package weixin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, StateFileName)

	if got := LoginStatus(path); got != (LoginInfo{}) {
		t.Errorf("missing file: %+v", got)
	}

	const secret = "fake-wx-token-should-not-leak"
	data := `{"token":" ` + secret + ` ","get_updates_buf":"cursor-secret","context_tokens":{"u":"ctx-secret"},
"base_url":"https://assigned.example.test","replaced_config_token_sha256":"abc","ilink_bot_id":"bot@im.bot","ilink_user_id":"user@im.wechat"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LoginStatus(path)
	want := LoginInfo{LoggedIn: true, BotID: "bot@im.bot", UserID: "user@im.wechat", BaseURL: "https://assigned.example.test"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	if err := os.WriteFile(path, []byte(`{"token":"`+secret), 0o600); err != nil {
		t.Fatal(err)
	}
	got = LoginStatus(path)
	if got.LoggedIn || got.Error == "" || strings.Contains(got.Error, secret) || strings.Contains(got.Error, dir) {
		t.Errorf("corrupt: %+v", got)
	}
}
