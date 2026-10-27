package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/yedamao/nanobot-go/internal/config"
	"github.com/yedamao/nanobot-go/internal/security"
)

// OAuthCallbackPath is where the authorization server sends the browser
// back to the gateway, as in nanobot.
const OAuthCallbackPath = "/auth/mcp/callback"

// defaultOAuthRedirectURI stands in when a stored entry has none.
const defaultOAuthRedirectURI = "http://127.0.0.1" + OAuthCallbackPath

// oauthHTTPTimeout bounds each request to the authorization server.
const oauthHTTPTimeout = 30 * time.Second

// maxOAuthMetadata caps a sniffed authorization server metadata body.
const maxOAuthMetadata = 1 << 20

// Client registration metadata, like nanobot's (RFC 7591).
const (
	oauthClientName = "nanobot-go"
	oauthClientURI  = "https://github.com/yedamao/nanobot-go"
	oauthLogoURI    = "https://raw.githubusercontent.com/yedamao/nanobot-go/main/webui/public/icon-192.png"
)

// OAuthBrowser is given only to a user-started authorization attempt
// (WithOAuthBrowser): with it, a server without valid credentials is
// sent through the browser authorization code flow.
type OAuthBrowser struct {
	// RedirectURI is the callback registered with the authorization
	// server; it ends in OAuthCallbackPath.
	RedirectURI string
	// Fetch hands the authorization URL to the user and waits for the
	// callback's code and state.
	Fetch auth.AuthorizationCodeFetcher
	// Reset drops stored tokens and the registered client first.
	Reset bool
}

// oauthAuth authorizes one connection's HTTP requests with the server's
// stored tokens and, for a browser attempt, the SDK's authorization code
// handler.
type oauthAuth struct {
	name      string
	serverURL string
	binding   *oauthBinding
	client    *http.Client // reaches the authorization server
	sniff     *metadataSniffer
	handler   *auth.AuthorizationCodeHandler // nil in the background

	authMu sync.Mutex // serializes authorize

	tsMu sync.Mutex
	ts   *oauthTokenSource // nil until there is a token
}

// newOAuth prepares OAuth for one connection attempt to name. Without a
// browser and without stored credentials it fails with
// ErrAuthorizationRequired before any network request: background
// connections never discover, register or prompt.
func (m *Manager) newOAuth(name string, cfg config.MCPServerConfig) (*oauthAuth, error) {
	if m.oauth == nil {
		return nil, fmt.Errorf("%w (no credential store)", ErrAuthorizationRequired)
	}
	b := m.oauth.bind(name, cfg.URL)
	sniff := &metadataSniffer{}
	a := &oauthAuth{name: name, serverURL: cfg.URL, binding: b, sniff: sniff}
	a.client = oauthHTTPClient(m.ssrf, sniff)

	browser := m.browsers[name]
	if browser != nil {
		if err := b.prepareRedirectURI(browser.RedirectURI, browser.Reset); err != nil {
			return nil, err
		}
	}
	snap, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	if tok := snap.token(); tok != nil {
		a.ts = &oauthTokenSource{a: a, cfg: oauthConfigOf(snap), tok: tok}
	}
	if browser == nil {
		if a.ts == nil {
			return nil, ErrAuthorizationRequired
		}
		return a, nil
	}

	hcfg := &auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				RedirectURIs:            []string{browser.RedirectURI},
				TokenEndpointAuthMethod: "none",
				GrantTypes:              []string{"authorization_code", "refresh_token"},
				ResponseTypes:           []string{"code"},
				ClientName:              oauthClientName,
				ClientURI:               oauthClientURI,
				LogoURI:                 oauthLogoURI,
				SoftwareID:              oauthClientURI,
			},
		},
		RedirectURL:              browser.RedirectURI,
		AuthorizationCodeFetcher: browser.Fetch,
		RequestRefreshToken:      true,
		Client:                   a.client,
		NewTokenSource:           a.newTokenSource,
	}
	// Reuse the client registered for this redirect URI (it was dropped
	// if the URI changed).
	if c := snap.client; c != nil && c.ClientID != "" {
		pre := &oauthex.ClientCredentials{ClientID: c.ClientID, Issuer: snap.oauthIssuer}
		if c.ClientSecret != "" {
			pre.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: c.ClientSecret}
		}
		hcfg.PreregisteredClient = pre
	}
	h, err := auth.NewAuthorizationCodeHandler(hcfg)
	if err != nil {
		return nil, err
	}
	a.handler = h
	return a, nil
}

// wrapOAuth makes an HTTP transport's requests carry a's tokens.
// Other transports (e.g. in-memory ones in tests) are left alone.
func wrapOAuth(t sdk.Transport, a *oauthAuth) {
	wrap := func(c *http.Client) *http.Client {
		if c == nil {
			c = http.DefaultClient
		}
		out := *c
		base := c.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		out.Transport = &oauthTransport{base: base, a: a}
		return &out
	}
	switch t := t.(type) {
	case *sdk.StreamableClientTransport:
		t.HTTPClient = wrap(t.HTTPClient)
	case *sdk.SSEClientTransport:
		t.HTTPClient = wrap(t.HTTPClient)
	}
}

// oauthHTTPClient reaches authorization servers under the SSRF policy,
// without the MCP server's headers, recording their metadata.
func oauthHTTPClient(policy *security.Policy, sniff *metadataSniffer) *http.Client {
	base := httpClient(nil, policy)
	sniff.base = base.Transport
	return &http.Client{Transport: sniff, CheckRedirect: base.CheckRedirect, Timeout: oauthHTTPTimeout}
}

func (a *oauthAuth) tokenSource() *oauthTokenSource {
	a.tsMu.Lock()
	defer a.tsMu.Unlock()
	return a.ts
}

// token returns the token to send: nil (no header) when there is none
// yet and a browser can get one.
func (a *oauthAuth) token() (*oauth2.Token, error) {
	ts := a.tokenSource()
	if ts == nil {
		if a.handler != nil {
			return nil, nil
		}
		return nil, ErrAuthorizationRequired
	}
	tok, err := ts.Token()
	if err != nil && a.handler != nil && errors.Is(err, ErrAuthorizationRequired) {
		return nil, nil // let the 401 start the browser flow
	}
	return tok, err
}

// newTokenSource is the SDK handler's NewTokenSource: it stores the new
// authorization (metadata, client and tokens) and refreshes from it.
func (a *oauthAuth) newTokenSource(_ context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
	meta := a.sniff.find(cfg.Endpoint.TokenURL)
	if meta == nil {
		// The 2025-03-26 fallback: endpoints under the server's origin.
		meta = &storedMetadata{
			Issuer:                strings.TrimSuffix(cfg.Endpoint.TokenURL, "/token"),
			AuthorizationEndpoint: cfg.Endpoint.AuthURL,
			TokenEndpoint:         cfg.Endpoint.TokenURL,
		}
	}
	client := &storedClient{
		ClientID:                cfg.ClientID,
		ClientSecret:            cfg.ClientSecret,
		TokenEndpointAuthMethod: authMethodOf(cfg),
		RedirectURIs:            []string{cfg.RedirectURL},
	}
	ok, err := a.binding.saveAuthorization(meta, meta.Issuer, client, tok)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("MCP authorization was cancelled: %w", ErrAuthorizationRequired)
	}
	c := *cfg
	ts := &oauthTokenSource{a: a, cfg: &c, tok: tok}
	a.tsMu.Lock()
	a.ts = ts
	a.tsMu.Unlock()
	return ts, nil
}

// authorize handles a 401 (or a 403 asking for more scope) that a token
// refresh did not fix. In the background it drops the rejected tokens and
// reports ErrAuthorizationRequired; a browser attempt runs the SDK's
// authorization code flow. It closes resp's body.
func (a *oauthAuth) authorize(ctx context.Context, resp *http.Response, used *oauth2.Token) error {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	if ts := a.tokenSource(); ts != nil {
		if cur := ts.current(); cur != nil && (used == nil || cur.AccessToken != used.AccessToken) {
			drain(resp)
			return nil // authorized meanwhile
		}
	}
	if a.handler == nil {
		drain(resp)
		if _, err := a.binding.clearTokens(); err != nil {
			return err
		}
		return ErrAuthorizationRequired
	}
	// The resource is the configured server URL, not an SSE message URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.serverURL, nil)
	if err != nil {
		drain(resp)
		return err
	}
	return a.handler.Authorize(ctx, req, resp)
}

// oauthTransport adds the bearer token to every request and, on a 401,
// refreshes the token once or authorizes, then retries once.
type oauthTransport struct {
	base http.RoundTripper
	a    *oauthAuth
}

func (t *oauthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil && r.Body != http.NoBody && r.GetBody == nil {
		data, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return nil, err
		}
		r = r.Clone(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	}
	send := func(tok *oauth2.Token) (*http.Response, error) {
		req := r.Clone(r.Context()) // a RoundTripper must not modify its request
		if r.GetBody != nil {
			body, err := r.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
		if tok != nil {
			tok.SetAuthHeader(req)
		}
		return t.base.RoundTrip(req)
	}
	fail := func(err error) (*http.Response, error) {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, err
	}

	tok, err := t.a.token()
	if err != nil {
		return fail(err)
	}
	resp, err := send(tok)
	if err != nil || !needsAuthorization(resp) {
		return resp, err
	}
	// A server may reject a token before its expiry: refresh it once.
	if tok != nil && tok.RefreshToken != "" {
		if ts := t.a.tokenSource(); ts != nil {
			fresh, rerr := ts.refreshAfterRejection(tok)
			switch {
			case rerr == nil:
				drain(resp)
				resp, err = send(fresh)
				if err != nil || !needsAuthorization(resp) {
					return resp, err
				}
				tok = fresh
			case !errors.Is(rerr, ErrAuthorizationRequired):
				drain(resp)
				return fail(rerr)
			}
		}
	}
	if err := t.a.authorize(r.Context(), resp, tok); err != nil {
		return fail(err)
	}
	if tok, err = t.a.token(); err != nil {
		return fail(err)
	}
	if tok == nil {
		return fail(ErrAuthorizationRequired)
	}
	return send(tok)
}

// needsAuthorization reports a 401, or a 403 asking for more scope (step
// up authorization).
func needsAuthorization(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return true
	case http.StatusForbidden:
		return strings.Contains(strings.Join(resp.Header.Values("WWW-Authenticate"), ","), "insufficient_scope")
	}
	return false
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

// oauthTokenSource hands out the stored access token and refreshes it
// when it expires, as nanobot's _RefreshingOAuthClientProvider does: one
// refresh at a time per server across processes, adopting tokens another
// process refreshed meanwhile, and committing only if the stored tokens
// are still the ones it started from. A refresh the authorization server
// refuses (invalid_grant, invalid_client, unauthorized_client) clears the
// tokens; other failures keep them for a later retry.
type oauthTokenSource struct {
	a *oauthAuth

	mu  sync.Mutex
	cfg *oauth2.Config // nil: no stored endpoint to refresh with
	tok *oauth2.Token
}

func (s *oauthTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok.Valid() {
		return s.tok, nil
	}
	return s.refreshLocked()
}

func (s *oauthTokenSource) current() *oauth2.Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tok
}

// refreshAfterRejection refreshes after the server rejected used, unless
// the token changed meanwhile.
func (s *oauthTokenSource) refreshAfterRejection(used *oauth2.Token) (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != nil && s.tok.AccessToken != used.AccessToken && s.tok.Valid() {
		return s.tok, nil
	}
	return s.refreshLocked()
}

func (s *oauthTokenSource) refreshLocked() (*oauth2.Token, error) {
	if s.tok == nil || s.tok.RefreshToken == "" {
		return nil, ErrAuthorizationRequired
	}
	b := s.a.binding
	unlock, err := b.refreshLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	snap, err := b.snapshot()
	if err != nil {
		return nil, err
	}
	stored := snap.token()
	if stored == nil {
		s.tok = nil
		return nil, ErrAuthorizationRequired
	}
	if stored.AccessToken != s.tok.AccessToken || stored.RefreshToken != s.tok.RefreshToken {
		// Refreshed or re-authorized elsewhere.
		s.tok, s.cfg = stored, oauthConfigOf(snap)
		if stored.Valid() {
			return stored, nil
		}
		if stored.RefreshToken == "" {
			return nil, ErrAuthorizationRequired
		}
	}
	// Tokens are only refreshed at the issuer that granted them.
	if s.cfg == nil || snap.oauthIssuer == "" || snap.tokenIssuer != normalizeIssuer(snap.oauthIssuer) {
		if _, err := b.clearTokens(); err != nil {
			return nil, err
		}
		s.tok = nil
		return nil, ErrAuthorizationRequired
	}
	lease := refreshLease{s.tok.AccessToken, s.tok.RefreshToken, snap.tokenIssuer}

	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), oauth2.HTTPClient, s.a.client), oauthHTTPTimeout)
	defer cancel()
	fresh, err := s.cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: s.tok.RefreshToken}).Token()
	if err != nil {
		var re *oauth2.RetrieveError
		if !errors.As(err, &re) {
			return nil, fmt.Errorf("MCP OAuth token refresh failed; will retry later: %w", err)
		}
		switch re.ErrorCode {
		case "invalid_grant", "invalid_client", "unauthorized_client":
			cleared, err := b.finishRefresh(lease, nil, re.ErrorCode != "invalid_grant")
			if err != nil {
				return nil, err
			}
			if !cleared {
				return s.reloadAfterLostLease(lease)
			}
			s.tok = nil
			return nil, fmt.Errorf("%w (token refresh: %s)", ErrAuthorizationRequired, re.ErrorCode)
		}
		status := 0
		if re.Response != nil {
			status = re.Response.StatusCode
		}
		if status == http.StatusNotFound {
			// The token endpoint moved: rediscover on the next sign-in.
			if _, err := b.clearMetadata(); err != nil {
				return nil, err
			}
			s.cfg = nil
			return nil, fmt.Errorf("%w (token endpoint not found)", ErrAuthorizationRequired)
		}
		return nil, fmt.Errorf("MCP OAuth token refresh failed (HTTP %d); will retry later", status)
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = s.tok.RefreshToken
	}
	if _, ok := fresh.Extra("scope").(string); !ok {
		if sc, ok := s.tok.Extra("scope").(string); ok {
			fresh = fresh.WithExtra(map[string]any{"scope": sc})
		}
	}
	committed, err := b.finishRefresh(lease, fresh, false)
	if err != nil {
		return nil, err
	}
	if !committed {
		return s.reloadAfterLostLease(lease)
	}
	s.tok = fresh
	return fresh, nil
}

// reloadAfterLostLease adopts the tokens that replaced lease's.
func (s *oauthTokenSource) reloadAfterLostLease(lease refreshLease) (*oauth2.Token, error) {
	snap, err := s.a.binding.snapshot()
	if err != nil {
		return nil, err
	}
	tok := snap.token()
	s.tok, s.cfg = tok, oauthConfigOf(snap)
	if tok == nil {
		return nil, ErrAuthorizationRequired
	}
	if tok.AccessToken != lease.accessToken || tok.RefreshToken != lease.refreshToken {
		return tok, nil
	}
	return nil, errors.New("MCP OAuth credentials changed while refreshing; retry later")
}

// oauthConfigOf rebuilds the refresh configuration from the store; nil
// when the client or token endpoint is unknown.
func oauthConfigOf(snap oauthSnapshot) *oauth2.Config {
	if snap.client == nil || snap.client.ClientID == "" || snap.metadata == nil || snap.metadata.TokenEndpoint == "" {
		return nil
	}
	style := oauth2.AuthStyleAutoDetect
	switch snap.client.TokenEndpointAuthMethod {
	case "none", "client_secret_post":
		style = oauth2.AuthStyleInParams
	case "client_secret_basic":
		style = oauth2.AuthStyleInHeader
	}
	redirect := snap.redirectURI
	if redirect == "" {
		redirect = defaultOAuthRedirectURI
	}
	return &oauth2.Config{
		ClientID:     snap.client.ClientID,
		ClientSecret: snap.client.ClientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:   snap.metadata.AuthorizationEndpoint,
			TokenURL:  snap.metadata.TokenEndpoint,
			AuthStyle: style,
		},
		RedirectURL: redirect,
	}
}

func authMethodOf(cfg *oauth2.Config) string {
	switch cfg.Endpoint.AuthStyle {
	case oauth2.AuthStyleInParams:
		if cfg.ClientSecret == "" {
			return "none"
		}
		return "client_secret_post"
	case oauth2.AuthStyleInHeader:
		return "client_secret_basic"
	}
	if cfg.ClientSecret == "" {
		return "none"
	}
	return ""
}

// metadataSniffer records the authorization server metadata the SDK's
// handler discovers, which it does not expose, so it can be stored for
// refreshing without rediscovery.
type metadataSniffer struct {
	base  http.RoundTripper
	mu    sync.Mutex
	metas []storedMetadata
}

func (s *metadataSniffer) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.base.RoundTrip(r)
	if err != nil || r.Method != http.MethodGet || resp.StatusCode != http.StatusOK ||
		!(strings.Contains(r.URL.Path, "/.well-known/oauth-authorization-server") || strings.Contains(r.URL.Path, "/.well-known/openid-configuration")) {
		return resp, err
	}
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, maxOAuthMetadata))
	resp.Body.Close()
	if rerr != nil {
		return nil, rerr
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	var m storedMetadata
	if json.Unmarshal(data, &m) == nil && m.Issuer != "" && m.TokenEndpoint != "" {
		s.mu.Lock()
		s.metas = append(s.metas, m)
		s.mu.Unlock()
	}
	return resp, nil
}

// find returns the last metadata naming tokenURL.
func (s *metadataSniffer) find(tokenURL string) *storedMetadata {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.metas) - 1; i >= 0; i-- {
		if s.metas[i].TokenEndpoint == tokenURL {
			m := s.metas[i]
			return &m
		}
	}
	return nil
}
