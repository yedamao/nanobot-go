package weixin

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	// maxQRRefresh is how often an expired QR code is replaced before the
	// login gives up (nanobot: MAX_QR_REFRESH_COUNT).
	maxQRRefresh = 3
	// qrPollInterval is the wait between get_qrcode_status calls.
	qrPollInterval = time.Second
	// maxLocalTokens caps local_token_list.
	maxLocalTokens = 10
)

// QR login statuses (get_qrcode_status), spelled as iLink spells them.
const (
	qrWait          = "wait"
	qrScanned       = "scaned"
	qrConfirmed     = "confirmed"
	qrExpired       = "expired"
	qrRedirect      = "scaned_but_redirect"
	qrNeedVerify    = "need_verifycode"
	qrVerifyBlocked = "verify_code_blocked"
	qrBound         = "binded_redirect"
)

// LoginOptions configure Login.
type LoginOptions struct {
	// Config is the parsed channels.weixin section; its BaseURL serves the
	// QR code.
	Config Config
	// StatePath is account.json (Config.StatePath).
	StatePath string
	// Force logs in again even when a login is saved, and accepts only
	// new credentials. The saved login is kept until the new one is
	// confirmed.
	Force bool
	// Out gets the QR code link and the progress; In answers verification
	// prompts (nil: none can be answered).
	Out io.Writer
	In  io.Reader
	// HTTPClient calls iLink; nil builds one that ignores the proxy
	// environment.
	HTTPClient *http.Client
	// Version goes into the request headers' bot agent.
	Version string
	// Sleep waits between status polls; nil uses a timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// LoginResult describes a successful Login.
type LoginResult struct {
	// AlreadyLoggedIn is set when nothing changed: a login was saved (and
	// Force was not set), or iLink reported the account already bound to
	// a token this host has.
	AlreadyLoggedIn bool
	// BotID and UserID are the IDs iLink reported; BaseURL is the API
	// host saved for the account.
	BotID, UserID, BaseURL string
}

// Login runs nanobot's QR-code login (connect.py, WeixinChannel._qr_login):
// it fetches a QR code, prints its link, polls get_qrcode_status until
// the scan is confirmed (following a redirect to another API host,
// prompting for a verification code when asked, replacing an expired code
// up to three times) and saves the bot token and its API host to
// account.json with mode 0600. The token is never printed, and never
// written to config.json.
func Login(ctx context.Context, opts LoginOptions) (LoginResult, error) {
	cfg := opts.Config
	sf := stateFile{opts.StatePath}
	st, err := sf.load()
	if err != nil {
		return LoginResult{}, err
	}
	if !opts.Force && st.Token != "" {
		return LoginResult{AlreadyLoggedIn: true, BotID: st.BotID, UserID: st.UserID,
			BaseURL: cmp.Or(st.BaseURL, cfg.BaseURL)}, nil
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	l := &login{api: newClient(opts.HTTPClient, string(cfg.RouteTag), opts.Version), base: cfg.BaseURL, out: out,
		localTokens: []string{}}
	if !opts.Force {
		l.localTokens = localTokens(cfg.Token, st.Token)
	}
	if opts.In != nil {
		l.in = bufio.NewReader(opts.In)
	}

	qr, err := l.fetchQR(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	pollBase, verify, refreshes, scanned := cfg.BaseURL, "", 0, false
	refresh := func(why string) error {
		if refreshes++; refreshes > maxQRRefresh {
			return fmt.Errorf("weixin login: %s too many times (%d); run the login again", why, maxQRRefresh)
		}
		fmt.Fprintf(out, "The QR code %s; here is a new one.\n", why)
		if qr, err = l.fetchQR(ctx); err != nil {
			return err
		}
		pollBase, verify, scanned = cfg.BaseURL, "", false
		return nil
	}
	for {
		var s qrStatusResponse
		q := url.Values{"qrcode": {qr}}
		if verify != "" {
			q.Set("verify_code", verify)
		}
		err := l.api.do(ctx, request{method: http.MethodGet, base: pollBase, endpoint: "ilink/bot/get_qrcode_status",
			query: q, timeout: qrStatusTimeout}, &s)
		if err != nil {
			if ctx.Err() != nil {
				return LoginResult{}, fmt.Errorf("weixin login: %w", ctx.Err())
			}
			if !retryableRequest(err) {
				return LoginResult{}, fmt.Errorf("weixin login: %w", err)
			}
			if err := sleep(ctx, qrPollInterval); err != nil {
				return LoginResult{}, fmt.Errorf("weixin login: %w", err)
			}
			continue
		}
		switch s.Status {
		case qrConfirmed:
			return l.commit(sf, cfg, s)
		case qrScanned:
			if !scanned {
				scanned = true
				fmt.Fprintln(out, "Scanned. Confirm the login in WeChat.")
			}
		case qrRedirect:
			if base := redirectBase(s.RedirectHost); base != "" {
				pollBase = base
			}
		case qrNeedVerify:
			prompt := "Enter the number shown in WeChat to continue: "
			if verify != "" {
				prompt = "The previous number did not match. Enter the number shown in WeChat: "
			}
			if verify, err = l.readLine(ctx, prompt); err != nil {
				return LoginResult{}, err
			}
			continue
		case qrVerifyBlocked:
			if err := refresh("verification was blocked"); err != nil {
				return LoginResult{}, err
			}
			continue
		case qrBound:
			switch {
			case opts.Force:
				return LoginResult{}, errors.New("weixin login: WeChat reported an existing binding instead of new credentials; scan with the account you want to connect")
			case st.Token != "" || cfg.Token != "":
				return LoginResult{AlreadyLoggedIn: true, BaseURL: cmp.Or(st.BaseURL, cfg.BaseURL)}, nil
			}
			return LoginResult{}, errors.New("weixin login: WeChat reported an existing binding, but this host has no saved token; run the login again with --force")
		case qrExpired:
			if err := refresh("expired"); err != nil {
				return LoginResult{}, err
			}
			continue
		}
		// wait, or a status this port does not know: keep polling.
		if err := sleep(ctx, qrPollInterval); err != nil {
			return LoginResult{}, fmt.Errorf("weixin login: %w", err)
		}
	}
}

// login is the state of one Login.
type login struct {
	api         *client
	base        string
	out         io.Writer
	in          *bufio.Reader
	localTokens []string
}

// fetchQR asks for a QR code and prints its link. When the server
// rejects the local tokens it is asked again without them (nanobot:
// _fetch_qr_code).
func (l *login) fetchQR(ctx context.Context) (string, error) {
	get := func(tokens []string) (qrCodeResponse, error) {
		var r qrCodeResponse
		err := l.api.do(ctx, request{method: http.MethodPost, base: l.base, endpoint: "ilink/bot/get_bot_qrcode",
			query: url.Values{"bot_type": {"3"}}, body: qrCodeRequest{LocalTokenList: tokens}, timeout: apiTimeout}, &r)
		return r, err
	}
	r, err := get(l.localTokens)
	if err != nil && len(l.localTokens) > 0 && isAPIError(err, errcodeInvalidArgument) {
		// The server rejected the saved credentials; never send them again
		// in this login.
		l.localTokens = []string{}
		r, err = get(l.localTokens)
	}
	if err != nil {
		return "", fmt.Errorf("weixin login: get a QR code: %w", err)
	}
	if r.QRCode == "" {
		return "", errors.New("weixin login: the server returned no QR code")
	}
	link := cmp.Or(r.QRCodeImgContent, r.QRCode)
	fmt.Fprintf(l.out, "\nScan this QR code with WeChat to log in. Render the link as a QR code, for example with\n\n  qrencode -t ansiutf8 '%s'\n\nLink: %s\n\nWaiting for the scan (Ctrl-C to cancel)...\n", link, link)
	return r.QRCode, nil
}

// readLine prompts for and reads one line of input.
func (l *login) readLine(ctx context.Context, prompt string) (string, error) {
	if l.in == nil {
		return "", errors.New("weixin login: WeChat asks for a verification code, but there is no input to read it from")
	}
	fmt.Fprint(l.out, prompt)
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := l.in.ReadString('\n')
		ch <- result{line, err}
	}()
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("weixin login: %w", ctx.Err())
	case r := <-ch:
		line := strings.TrimSpace(r.line)
		if line == "" && r.err != nil {
			return "", fmt.Errorf("weixin login: read the verification code: %w", r.err)
		}
		return line, nil
	}
}

// commit saves a confirmed login (nanobot: _commit_account). The cursor
// and context tokens of the previous token are dropped. When the login
// replaces a configured token, its fingerprint is kept so the new token
// wins over it until the configuration changes.
func (l *login) commit(sf stateFile, cfg Config, s qrStatusResponse) (LoginResult, error) {
	token := strings.TrimSpace(s.BotToken)
	if token == "" {
		return LoginResult{}, errors.New("weixin login: WeChat confirmed the scan but returned no token")
	}
	if !validToken(token) {
		return LoginResult{}, errors.New("weixin login: WeChat returned a malformed token")
	}
	base := cfg.BaseURL
	if b := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/"); b != "" && checkBaseURL(b) == nil {
		base = b
	}
	replaced := ""
	if cfg.Token != "" && cfg.Token != token {
		replaced = tokenFingerprint(cfg.Token)
	}
	err := sf.update(func(st *accountState) bool {
		*st = accountState{Token: token, BaseURL: base, ReplacedConfigTokenSHA256: replaced,
			BotID: s.BotID, UserID: s.UserID}
		return true
	})
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{BotID: s.BotID, UserID: s.UserID, BaseURL: base}, nil
}

// localTokens returns the distinct non-empty tokens, at most
// maxLocalTokens.
func localTokens(tokens ...string) []string {
	out := []string{}
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t != "" && !slices.Contains(out, t) && len(out) < maxLocalTokens {
			out = append(out, t)
		}
	}
	return out
}

// redirectBase turns a redirect_host into an API base URL, or "" when it
// is not a usable http(s) host.
func redirectBase(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "https://" + host
	}
	host = strings.TrimRight(host, "/")
	if checkBaseURL(host) != nil {
		return ""
	}
	return host
}
