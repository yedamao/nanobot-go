package mcp

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// The browser flows port nanobot/webui/mcp_oauth_api.py: the gateway owns
// short-lived authorization attempts that the WebUI starts, polls,
// completes (by the callback landing on OAuthCallbackPath, or a pasted
// callback URL) and cancels.

const (
	// OAuthFlowTTL is how long a browser flow waits for its callback.
	OAuthFlowTTL = 5 * time.Minute
	// oauthStartWait bounds how long Start waits for the authorization
	// URL (or for the flow to finish without one).
	oauthStartWait = 20 * time.Second
	// maxOAuthCode caps an authorization code.
	maxOAuthCode = 8192
	// maxOAuthState caps the state in an authorization URL.
	maxOAuthState = 512
	// maxCallbackFields caps the query fields of a pasted callback URL.
	maxCallbackFields = 16
	// oauthCancelWait bounds how long Cancel waits for the attempt to end.
	oauthCancelWait = 10 * time.Second
)

// Flow statuses, as nanobot reports them.
const (
	OAuthStarting              = "starting"
	OAuthAuthorizationRequired = "authorization_required"
	OAuthConnecting            = "connecting"
	OAuthAuthorized            = "authorized"
	OAuthConnected             = "connected"
	OAuthFailed                = "failed"
	OAuthCancelled             = "cancelled"
)

var oauthErrorRE = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,80}$`)

// OAuthFlowError is a safe message for the WebUI with its HTTP status.
type OAuthFlowError struct {
	Message string
	Status  int
}

func (e *OAuthFlowError) Error() string { return e.Message }

func flowErr(status int, msg string) error { return &OAuthFlowError{Message: msg, Status: status} }

// HotReload reports activating a server after its sign-in.
type HotReload struct {
	OK              bool   `json:"ok"`
	Message         string `json:"message,omitempty"`
	RequiresRestart bool   `json:"requires_restart,omitempty"`
}

// OAuthFlowStatus is a flow as the WebUI sees it.
type OAuthFlowStatus struct {
	FlowID string `json:"flow_id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// ExpiresIn is the seconds left to complete the flow.
	ExpiresIn int `json:"expires_in"`
	// CompletionInput is "callback_url" when the callback cannot reach
	// the gateway and the user must paste it (remote HTTP WebUI).
	CompletionInput  string     `json:"completion_input,omitempty"`
	AuthorizationURL string     `json:"authorization_url,omitempty"`
	RedirectURI      string     `json:"redirect_uri,omitempty"`
	Error            string     `json:"error,omitempty"`
	HotReload        *HotReload `json:"hot_reload,omitempty"`
}

// OAuthConnectFunc connects server name once through browser, with a
// throwaway Manager, and closes it: on success the credentials are
// stored.
type OAuthConnectFunc func(ctx context.Context, name string, browser *OAuthBrowser) error

// OAuthActivateFunc makes the running gateway use a server's new
// credentials.
type OAuthActivateFunc func(ctx context.Context, name string) HotReload

// OAuthFlows owns the gateway's browser flows. It is safe for concurrent
// use.
type OAuthFlows struct {
	connect  OAuthConnectFunc
	activate OAuthActivateFunc
	// checkAuthURL vets the authorization URL a server returns before the
	// WebUI opens it (nil: only its shape is checked).
	checkAuthURL func(context.Context, *url.URL) error
	// allowHTTPAuthURL accepts http authorization URLs (tests).
	allowHTTPAuthURL bool
	ttl              time.Duration
	now              func() time.Time

	mu     sync.Mutex
	flows  map[string]*oauthFlow // by flow ID
	states map[string]string     // OAuth state -> flow ID
	closed bool
	wg     sync.WaitGroup
}

type callbackResult struct {
	res *auth.AuthorizationResult
	err error
}

type oauthFlow struct {
	id, name    string
	redirectURI string
	manual      bool
	expiresAt   time.Time
	cancel      context.CancelFunc
	ready       chan struct{}       // closed once authURL is set
	callback    chan callbackResult // buffered 1; written once
	done        chan struct{}       // closed when the attempt ends

	// Guarded by OAuthFlows.mu.
	authURL          string
	state            string
	callbackUsed     bool
	callbackReceived bool
	err              string
	finished         bool
	succeeded        bool
	cancelled        bool
	hot              *HotReload
}

// NewOAuthFlows returns the flow owner. checkAuthURL may be nil.
func NewOAuthFlows(connect OAuthConnectFunc, activate OAuthActivateFunc, checkAuthURL func(context.Context, *url.URL) error) *OAuthFlows {
	return &OAuthFlows{
		connect: connect, activate: activate, checkAuthURL: checkAuthURL,
		ttl: OAuthFlowTTL, now: time.Now,
		flows: map[string]*oauthFlow{}, states: map[string]string{},
	}
}

// Start begins authorizing server name with the callback at redirectURI
// (see PrepareOAuthRedirectURI), replacing the name's running flow. It
// waits briefly for the authorization URL, which the result then holds,
// or for the attempt to finish without one (stored tokens still worked).
func (f *OAuthFlows) Start(name, redirectURI string, reset bool) (OAuthFlowStatus, error) {
	redirect, manual, err := PrepareOAuthRedirectURI(redirectURI)
	if err != nil {
		return OAuthFlowStatus{}, err
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return OAuthFlowStatus{}, flowErr(http.StatusServiceUnavailable, "The gateway is shutting down.")
	}
	f.pruneLocked()
	var old []*oauthFlow
	for _, fl := range f.flows {
		if fl.name == name && !fl.finished {
			old = append(old, fl)
		}
	}
	f.mu.Unlock()
	for _, fl := range old {
		f.cancelFlow(fl)
	}

	ctx, cancel := context.WithCancel(context.Background())
	fl := &oauthFlow{
		id: rand.Text(), name: name, redirectURI: redirect, manual: manual,
		expiresAt: f.now().Add(f.ttl), cancel: cancel,
		ready: make(chan struct{}), callback: make(chan callbackResult, 1), done: make(chan struct{}),
	}
	ctx, cancelTTL := context.WithDeadline(ctx, fl.expiresAt)
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		cancelTTL()
		cancel()
		return OAuthFlowStatus{}, flowErr(http.StatusServiceUnavailable, "The gateway is shutting down.")
	}
	f.flows[fl.id] = fl
	f.wg.Add(1)
	f.mu.Unlock()

	browser := &OAuthBrowser{
		RedirectURI: redirect,
		Reset:       reset,
		Fetch: func(fctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			return f.fetch(fctx, fl, args)
		},
	}
	go func() {
		defer f.wg.Done()
		defer close(fl.done)
		defer cancelTTL()
		err := f.connect(ctx, name, browser)
		var hot *HotReload
		if err == nil && ctx.Err() == nil {
			h := f.activate(context.WithoutCancel(ctx), name)
			hot = &h
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		fl.finished = true
		switch {
		case fl.cancelled:
		case err != nil || ctx.Err() != nil:
			if fl.err == "" {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					fl.err = "MCP authorization timed out."
				} else {
					fl.err = "Could not complete the MCP OAuth connection."
				}
			}
		default:
			fl.succeeded, fl.hot = true, hot
		}
	}()

	timer := time.NewTimer(oauthStartWait)
	defer timer.Stop()
	select {
	case <-fl.ready:
	case <-fl.done:
	case <-timer.C:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.payloadLocked(fl), nil
}

// fetch is the flow's AuthorizationCodeFetcher: it publishes the
// authorization URL and waits for the callback.
func (f *OAuthFlows) fetch(ctx context.Context, fl *oauthFlow, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	// The first failure is the one to report: a later request of the
	// same attempt may come back here.
	fail := func(msg string) (*auth.AuthorizationResult, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if fl.err == "" {
			fl.err = msg
		}
		return nil, errors.New(fl.err)
	}
	u, err := url.Parse(args.URL)
	if err != nil || (u.Scheme != "https" && !(f.allowHTTPAuthURL && u.Scheme == "http")) || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fail("The MCP server returned an unsafe authorization URL.")
	}
	if f.checkAuthURL != nil {
		if err := f.checkAuthURL(ctx, u); err != nil {
			return fail("The MCP server returned an unsafe authorization URL.")
		}
	}
	states := u.Query()["state"]
	if len(states) != 1 || states[0] == "" || len(states[0]) > maxOAuthState {
		return fail("The MCP server returned an invalid authorization URL.")
	}
	state := states[0]
	f.mu.Lock()
	if _, dup := f.states[state]; dup {
		f.mu.Unlock()
		return fail("The MCP server reused an OAuth state value.")
	}
	if fl.cancelled {
		f.mu.Unlock()
		return nil, context.Canceled
	}
	if fl.authURL != "" {
		f.mu.Unlock()
		return fail("The MCP server asked for authorization twice.")
	}
	fl.authURL, fl.state = args.URL, state
	f.states[state] = fl.id
	close(fl.ready)
	f.mu.Unlock()

	select {
	case r := <-fl.callback:
		return r.res, r.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fail("MCP authorization timed out.")
		}
		return nil, ctx.Err()
	}
}

// Status reports flow id of server name.
func (f *OAuthFlows) Status(name, id string) (OAuthFlowStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked()
	fl, err := f.flowLocked(name, id)
	if err != nil {
		return OAuthFlowStatus{}, err
	}
	return f.payloadLocked(fl), nil
}

// Callback delivers the authorization server's redirect (the query of
// OAuthCallbackPath) to its flow, and returns the flow's server name.
func (f *OAuthFlows) Callback(state, code, oauthError, iss string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked()
	return f.callbackLocked(state, code, oauthError, iss)
}

func (f *OAuthFlows) callbackLocked(state, code, oauthError, iss string) (string, error) {
	id, ok := f.states[state]
	if !ok {
		return "", flowErr(http.StatusGone, "This MCP authorization request has expired.")
	}
	delete(f.states, state)
	fl := f.flows[id]
	if fl == nil {
		return "", flowErr(http.StatusGone, "This MCP authorization request has expired.")
	}
	if fl.callbackUsed || fl.finished {
		return "", flowErr(http.StatusConflict, "This MCP authorization callback was already used.")
	}
	fl.callbackUsed, fl.callbackReceived = true, true
	switch {
	case oauthError != "":
		safe := oauthError
		if !oauthErrorRE.MatchString(safe) {
			safe = "authorization_failed"
		}
		fl.err = "Authorization was not completed (" + safe + ")."
		fl.callback <- callbackResult{err: errors.New(fl.err)}
		return "", flowErr(http.StatusBadRequest, fl.err)
	case code == "" || len(code) > maxOAuthCode:
		fl.err = "The MCP server did not return an authorization code."
		fl.callback <- callbackResult{err: errors.New(fl.err)}
		return "", flowErr(http.StatusBadRequest, fl.err)
	}
	fl.callback <- callbackResult{res: &auth.AuthorizationResult{Code: code, State: state, Iss: iss}}
	return fl.name, nil
}

// CompleteURL completes flow id with the full callback URL pasted from
// the browser's address bar, for a callback the gateway cannot receive.
func (f *OAuthFlows) CompleteURL(name, id, callbackURL string) (OAuthFlowStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneLocked()
	fl, err := f.flowLocked(name, id)
	if err != nil {
		return OAuthFlowStatus{}, err
	}
	paste := flowErr(http.StatusBadRequest, "Paste the complete callback URL from the browser address bar.")
	u, err := url.Parse(strings.TrimSpace(callbackURL))
	want, _ := url.Parse(fl.redirectURI)
	if err != nil || u.RawQuery == "" || u.Fragment != "" || u.User != nil ||
		u.Scheme != want.Scheme || u.Host != want.Host || u.Path != want.Path {
		return OAuthFlowStatus{}, paste
	}
	if strings.Count(u.RawQuery, "&")+1 > maxCallbackFields {
		return OAuthFlowStatus{}, paste
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return OAuthFlowStatus{}, paste
	}
	states := q["state"]
	if len(states) != 1 || states[0] == "" || states[0] != fl.state {
		return OAuthFlowStatus{}, flowErr(http.StatusGone, "This callback belongs to a different or expired authorization request. Start again.")
	}
	codes, errs, isss := q["code"], q["error"], q["iss"]
	if len(codes) > 1 || len(errs) > 1 || len(isss) > 1 || (len(codes) > 0 && len(errs) > 0) {
		return OAuthFlowStatus{}, paste
	}
	code, oauthError, iss := first(codes), first(errs), first(isss)
	if (code == "" && oauthError == "") || len(code) > maxOAuthCode {
		return OAuthFlowStatus{}, paste
	}
	if _, err := f.callbackLocked(states[0], code, oauthError, iss); err != nil {
		return OAuthFlowStatus{}, err
	}
	return f.payloadLocked(fl), nil
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// Cancel stops flow id of server name.
func (f *OAuthFlows) Cancel(name, id string) (OAuthFlowStatus, error) {
	f.mu.Lock()
	f.pruneLocked()
	fl, err := f.flowLocked(name, id)
	f.mu.Unlock()
	if err != nil {
		return OAuthFlowStatus{}, err
	}
	f.cancelFlow(fl)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.payloadLocked(fl), nil
}

// CancelName stops server name's running flows, e.g. when it is removed.
func (f *OAuthFlows) CancelName(name string) {
	f.mu.Lock()
	var running []*oauthFlow
	for _, fl := range f.flows {
		if fl.name == name && !fl.finished {
			running = append(running, fl)
		}
	}
	f.mu.Unlock()
	for _, fl := range running {
		f.cancelFlow(fl)
	}
}

func (f *OAuthFlows) cancelFlow(fl *oauthFlow) {
	f.mu.Lock()
	if fl.state != "" {
		delete(f.states, fl.state)
	}
	if !fl.finished {
		fl.cancelled = true
	}
	f.mu.Unlock()
	fl.cancel()
	select {
	case <-fl.done:
	case <-time.After(oauthCancelWait):
	}
}

// Close cancels every flow and waits for them.
func (f *OAuthFlows) Close() {
	f.mu.Lock()
	f.closed = true
	flows := make([]*oauthFlow, 0, len(f.flows))
	for _, fl := range f.flows {
		flows = append(flows, fl)
	}
	f.mu.Unlock()
	for _, fl := range flows {
		f.mu.Lock()
		if !fl.finished {
			fl.cancelled = true
		}
		f.mu.Unlock()
		fl.cancel()
	}
	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(oauthCancelWait):
	}
}

func (f *OAuthFlows) flowLocked(name, id string) (*oauthFlow, error) {
	fl := f.flows[id]
	if fl == nil || fl.name != name {
		return nil, flowErr(http.StatusNotFound, "Unknown or expired MCP OAuth flow.")
	}
	return fl, nil
}

// pruneLocked forgets expired flows; their attempts were ended by the
// TTL deadline.
func (f *OAuthFlows) pruneLocked() {
	now := f.now()
	for id, fl := range f.flows {
		if fl.expiresAt.After(now) {
			continue
		}
		if fl.state != "" {
			delete(f.states, fl.state)
		}
		fl.cancel()
		delete(f.flows, id)
	}
}

func (f *OAuthFlows) payloadLocked(fl *oauthFlow) OAuthFlowStatus {
	var status string
	switch {
	case fl.hot != nil && fl.hot.OK:
		status = OAuthConnected
	case fl.hot != nil:
		status = OAuthAuthorized
	case fl.cancelled:
		status = OAuthCancelled
	case fl.finished && !fl.succeeded:
		status = OAuthFailed
	case fl.finished:
		status = OAuthAuthorized
	case fl.callbackReceived:
		status = OAuthConnecting
	case fl.authURL != "":
		status = OAuthAuthorizationRequired
	default:
		status = OAuthStarting
	}
	out := OAuthFlowStatus{
		FlowID: fl.id, Name: fl.name, Status: status,
		ExpiresIn: max(0, int(fl.expiresAt.Sub(f.now()).Seconds())),
		Error:     fl.err, HotReload: fl.hot,
	}
	if fl.manual {
		out.CompletionInput = "callback_url"
		out.RedirectURI = fl.redirectURI
	}
	if status == OAuthAuthorizationRequired {
		out.AuthorizationURL = fl.authURL
	}
	if status == OAuthConnected || status == OAuthAuthorized {
		out.Error = ""
	}
	return out
}

// parseRedirectURI checks the shape of a callback URL: a host, the
// callback path and nothing else.
func parseRedirectURI(raw string) (*url.URL, error) {
	bad := flowErr(http.StatusBadRequest, "Invalid MCP OAuth callback URL.")
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Hostname() == "" || u.Path != OAuthCallbackPath ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil || u.Opaque != "" {
		return nil, bad
	}
	if p := u.Port(); p != "" {
		if _, err := net.LookupPort("tcp", p); err != nil {
			return nil, bad
		}
	}
	return u, nil
}

// ValidateOAuthRedirectURI accepts an HTTPS callback, or HTTP on a
// loopback host.
func ValidateOAuthRedirectURI(raw string) (string, error) {
	u, err := parseRedirectURI(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "https" || (u.Scheme == "http" && IsLoopbackHost(u.Hostname())) {
		return u.String(), nil
	}
	return "", flowErr(http.StatusBadRequest, "MCP OAuth callbacks must use HTTPS or localhost.")
}

// PrepareOAuthRedirectURI returns the callback to register for a WebUI
// reached at raw's origin. A remote WebUI over plain HTTP cannot receive
// the callback (authorization servers only redirect to HTTPS or
// loopback), so it gets http://127.0.0.1[:port]/auth/mcp/callback and
// manual is set: the user pastes the URL the browser lands on.
func PrepareOAuthRedirectURI(raw string) (redirect string, manual bool, err error) {
	u, err := parseRedirectURI(raw)
	if err != nil {
		return "", false, err
	}
	if u.Scheme != "http" || IsLoopbackHost(u.Hostname()) {
		redirect, err = ValidateOAuthRedirectURI(u.String())
		return redirect, false, err
	}
	host := "127.0.0.1"
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	redirect, err = ValidateOAuthRedirectURI((&url.URL{Scheme: "http", Host: host, Path: OAuthCallbackPath}).String())
	return redirect, true, err
}

// IsLoopbackHost reports localhost or a loopback IP (also IPv4-mapped).
func IsLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}
