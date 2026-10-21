package weixin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loginEnv is a fake iLink server for the QR login and a state file.
type loginEnv struct {
	f     *fakeILink
	state stateFile
	cfg   Config
	out   *lockedBuffer
	// statuses are the get_qrcode_status answers in order; the last one
	// repeats.
	statuses []string
}

func newLoginEnv(t *testing.T, statuses ...string) *loginEnv {
	t.Helper()
	e := &loginEnv{f: newFakeILink(t), out: &lockedBuffer{}, statuses: statuses}
	e.state = stateFile{filepath.Join(t.TempDir(), "state", "weixin", StateFileName)}
	e.cfg = Config{BaseURL: e.f.srv.URL, PollTimeout: DefaultPollTimeout}
	e.f.setReply(func(c apiCall, n int) (int, string, bool) {
		switch c.Endpoint {
		case "get_bot_qrcode":
			return 200, `{"ret":0,"qrcode":"qr-` + string(rune('0'+n)) + `","qrcode_img_content":"https://liteapp.example/q/qr-` + string(rune('0'+n)) + `"}`, true
		case "get_qrcode_status":
			s := e.statuses[min(n, len(e.statuses))-1]
			if strings.HasPrefix(s, "{") {
				return 200, s, true
			}
			return 200, `{"status":"` + s + `"}`, true
		}
		return 0, "", false
	})
	return e
}

func (e *loginEnv) login(ctx context.Context, force bool, in string) (LoginResult, error) {
	opts := LoginOptions{Config: e.cfg, StatePath: e.state.path, Force: force, Out: e.out,
		HTTPClient: e.f.srv.Client(), Version: "test",
		Sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
	if in != "" {
		opts.In = strings.NewReader(in)
	}
	return Login(ctx, opts)
}

const confirmed = `{"status":"confirmed","bot_token":"new-bot-SECRET","ilink_bot_id":"bot-1","ilink_user_id":"owner@im.wechat","baseurl":"https://assigned.example"}`

func TestLoginHappyPath(t *testing.T) {
	e := newLoginEnv(t, "wait", "scaned", confirmed)
	res, err := e.login(context.Background(), false, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.AlreadyLoggedIn || res.BotID != "bot-1" || res.UserID != "owner@im.wechat" || res.BaseURL != "https://assigned.example" {
		t.Errorf("result = %+v", res)
	}
	st, err := e.state.load()
	if err != nil || st.Token != "new-bot-SECRET" || st.BaseURL != "https://assigned.example" || st.BotID != "bot-1" {
		t.Errorf("state = %+v, %v", st, err)
	}
	fi, err := os.Stat(e.state.path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("account.json mode = %v, %v", fi.Mode(), err)
	}
	if di, err := os.Stat(filepath.Dir(e.state.path)); err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode = %v, %v", di.Mode(), err)
	}
	out := e.out.String()
	if !strings.Contains(out, "https://liteapp.example/q/qr-1") || !strings.Contains(out, "Scanned") || strings.Contains(out, "SECRET") {
		t.Errorf("output:\n%s", out)
	}
	// The QR code request: bot_type=3, no token, an empty local_token_list.
	qr := e.f.callsOf("get_bot_qrcode")[0]
	if qr.Query["bot_type"][0] != "3" || qr.Header.Get("Authorization") != "" {
		t.Errorf("get_bot_qrcode query %v, auth %q", qr.Query, qr.Header.Get("Authorization"))
	}
	if list, ok := qr.Body["local_token_list"].([]any); !ok || len(list) != 0 {
		t.Errorf("local_token_list = %#v", qr.Body["local_token_list"])
	}
	for _, c := range e.f.callsOf("get_qrcode_status") {
		if c.Query["qrcode"][0] != "qr-1" || c.Header.Get("Authorization") != "" {
			t.Errorf("status poll %v", c.Query)
		}
	}

	// A second login without --force keeps the saved one.
	before := len(e.f.callsOf("get_bot_qrcode"))
	res, err = e.login(context.Background(), false, "")
	if err != nil || !res.AlreadyLoggedIn || len(e.f.callsOf("get_bot_qrcode")) != before {
		t.Errorf("second login = %+v, %v", res, err)
	}
}

func TestLoginExpiry(t *testing.T) {
	// Refreshed QR codes work until the third refresh; the fourth expiry
	// gives up.
	e := newLoginEnv(t, "expired")
	_, err := e.login(context.Background(), false, "")
	if err == nil || !strings.Contains(err.Error(), "expired too many times") {
		t.Fatalf("Login = %v", err)
	}
	if n := len(e.f.callsOf("get_bot_qrcode")); n != maxQRRefresh+1 {
		t.Errorf("%d QR codes fetched", n)
	}
	if _, err := os.Stat(e.state.path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state written after a failed login: %v", err)
	}

	// An expired code is replaced and the new one polled.
	e = newLoginEnv(t, "expired", confirmed)
	if _, err := e.login(context.Background(), false, ""); err != nil {
		t.Fatal(err)
	}
	if polls := e.f.callsOf("get_qrcode_status"); polls[1].Query["qrcode"][0] != "qr-2" {
		t.Errorf("polled %v after the refresh", polls[1].Query)
	}
}

func TestLoginFollowsRedirectHost(t *testing.T) {
	other := newFakeILink(t)
	other.setReply(func(c apiCall, n int) (int, string, bool) { return 200, confirmed, true })
	host := strings.TrimPrefix(other.srv.URL, "http://")
	e := newLoginEnv(t, `{"status":"scaned_but_redirect","redirect_host":"http://`+host+`"}`)
	if _, err := e.login(context.Background(), false, ""); err != nil {
		t.Fatal(err)
	}
	if n := len(other.callsOf("get_qrcode_status")); n != 1 {
		t.Errorf("redirected host polled %d times", n)
	}
	if redirectBase("example.com") != "https://example.com" || redirectBase("") != "" || redirectBase("https://x/?q") != "" {
		t.Error("redirectBase")
	}
}

func TestLoginVerificationCode(t *testing.T) {
	e := newLoginEnv(t, "need_verifycode", confirmed)
	if _, err := e.login(context.Background(), false, "4321\n"); err != nil {
		t.Fatal(err)
	}
	polls := e.f.callsOf("get_qrcode_status")
	if len(polls) != 2 || polls[1].Query["verify_code"][0] != "4321" {
		t.Errorf("polls = %v", polls)
	}
	if !strings.Contains(e.out.String(), "Enter the number shown in WeChat") {
		t.Errorf("output:\n%s", e.out)
	}
	// Without input it fails clearly.
	e = newLoginEnv(t, "need_verifycode")
	if _, err := e.login(context.Background(), false, ""); err == nil || !strings.Contains(err.Error(), "verification code") {
		t.Errorf("Login = %v", err)
	}
}

func TestLoginRetriesTransientErrors(t *testing.T) {
	e := newLoginEnv(t, confirmed)
	e.f.setReply(func(c apiCall, n int) (int, string, bool) {
		switch {
		case c.Endpoint == "get_bot_qrcode":
			return 200, `{"ret":0,"qrcode":"qr-1"}`, true
		case n == 1:
			return 502, "bad gateway", true
		}
		return 200, confirmed, true
	})
	if _, err := e.login(context.Background(), false, ""); err != nil {
		t.Fatal(err)
	}
	// Without qrcode_img_content the QR code itself is the link.
	if !strings.Contains(e.out.String(), "Link: qr-1") {
		t.Errorf("output:\n%s", e.out)
	}
	e.f.setReply(func(c apiCall, n int) (int, string, bool) {
		if c.Endpoint == "get_bot_qrcode" {
			return 200, `{"ret":0,"qrcode":"qr-1"}`, true
		}
		return 403, "forbidden", true
	})
	os.Remove(e.state.path)
	if _, err := e.login(context.Background(), false, ""); err == nil {
		t.Error("Login succeeded on 403")
	}
}

func TestLoginForceAndLocalTokens(t *testing.T) {
	e := newLoginEnv(t, confirmed)
	e.cfg.Token = "configured-SECRET"
	e.state.update(func(st *accountState) bool { st.Token = "saved-SECRET"; return true })

	// --force sends no local tokens and replaces the saved login.
	if _, err := e.login(context.Background(), true, ""); err != nil {
		t.Fatal(err)
	}
	if list := e.f.callsOf("get_bot_qrcode")[0].Body["local_token_list"].([]any); len(list) != 0 {
		t.Errorf("forced login sent local tokens %v", list)
	}
	st, _ := e.state.load()
	if st.Token != "new-bot-SECRET" || st.ReplacedConfigTokenSHA256 != tokenFingerprint("configured-SECRET") {
		t.Errorf("state = %+v", st)
	}

	// Without --force (and no saved login) the configured token is sent;
	// when the server rejects it, the request is repeated without it.
	os.Remove(e.state.path)
	e.f.setReply(func(c apiCall, n int) (int, string, bool) {
		switch c.Endpoint {
		case "get_bot_qrcode":
			if list := c.Body["local_token_list"].([]any); len(list) > 0 {
				return 200, `{"ret":-3,"errmsg":"invalid"}`, true
			}
			return 200, `{"ret":0,"qrcode":"qr-x"}`, true
		}
		return 200, confirmed, true
	})
	if _, err := e.login(context.Background(), false, ""); err != nil {
		t.Fatal(err)
	}
	calls := e.f.callsOf("get_bot_qrcode")
	first := calls[len(calls)-2].Body["local_token_list"].([]any)
	if len(first) != 1 || first[0] != "configured-SECRET" || len(calls[len(calls)-1].Body["local_token_list"].([]any)) != 0 {
		t.Errorf("local token lists: %v then %v", first, calls[len(calls)-1].Body)
	}
}

func TestLoginExistingBinding(t *testing.T) {
	e := newLoginEnv(t, "binded_redirect")
	e.state.update(func(st *accountState) bool { st.Token = "saved-SECRET"; return true })
	if _, err := e.login(context.Background(), true, ""); err == nil || !strings.Contains(err.Error(), "existing binding") {
		t.Errorf("forced login = %v", err)
	}
	if st, _ := e.state.load(); st.Token != "saved-SECRET" {
		t.Error("a failed forced login dropped the saved one")
	}
	os.Remove(e.state.path)
	if _, err := e.login(context.Background(), false, ""); err == nil || !strings.Contains(err.Error(), "no saved token") {
		t.Errorf("login without a token = %v", err)
	}
}

func TestLoginCanceled(t *testing.T) {
	e := newLoginEnv(t, "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	opts := LoginOptions{Config: e.cfg, StatePath: e.state.path, Out: e.out, HTTPClient: e.f.srv.Client()}
	if _, err := Login(ctx, opts); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Login = %v", err)
	}
}

func TestLoginRejectsBadResponses(t *testing.T) {
	for name, body := range map[string]string{
		"no token":        `{"status":"confirmed"}`,
		"malformed token": `{"status":"confirmed","bot_token":"a b"}`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newLoginEnv(t, body)
			if _, err := e.login(context.Background(), false, ""); err == nil {
				t.Error("Login succeeded")
			}
		})
	}
	// An unusable baseurl keeps the configured one.
	e := newLoginEnv(t, `{"status":"confirmed","bot_token":"tok","baseurl":"ftp://x"}`)
	if res, err := e.login(context.Background(), false, ""); err != nil || res.BaseURL != e.cfg.BaseURL {
		t.Errorf("result = %+v, %v", res, err)
	}
	// A server without a QR code.
	e = newLoginEnv(t, "wait")
	e.f.setReply(func(c apiCall, n int) (int, string, bool) { return 200, `{"ret":0}`, true })
	if _, err := e.login(context.Background(), false, ""); err == nil || !strings.Contains(err.Error(), "no QR code") {
		t.Errorf("Login = %v", err)
	}
}
