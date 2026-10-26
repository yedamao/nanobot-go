package security

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// IsHTTPURL reports whether s is a remote media reference: it starts with
// http:// or https:// (nanobot's media_path.startswith check, ignoring
// case).
func IsHTTPURL(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// URLName returns the file name of a URL: the last segment of its path,
// unescaped, or its host when the path has none.
func URLName(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return path.Base(raw)
	}
	if name := path.Base(u.Path); name != "." && name != "/" && name != "" {
		return name
	}
	if u.Host != "" {
		return u.Host
	}
	return raw
}

// ErrTooLarge is returned (wrapped) by Download when the body exceeds its
// limit.
var ErrTooLarge = errors.New("file too large")

const (
	downloadRedirects = 5
	downloadTimeout   = 2 * time.Minute
)

// Download fetches rawURL with p (nil means the default policy) and
// returns at most maxBytes of its body, failing with ErrTooLarge beyond
// that. The target and every redirect hop are checked, and the
// connections only reach the addresses checked (see DialContext), so
// the URL cannot be used to reach internal hosts.
func (p *Policy) Download(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	ctx, cancel := context.WithTimeout(WithPinnedDNS(ctx), downloadTimeout)
	defer cancel()
	if _, err := p.CheckTarget(ctx, u); err != nil {
		return nil, err
	}
	transport := p.NewTransport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: p.CheckRedirect(downloadRedirects)}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("download %s: %w", u.Redacted(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("download %s: HTTP %d", u.Redacted(), resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("%w (%d bytes, limit %d)", ErrTooLarge, resp.ContentLength, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u.Redacted(), err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w (limit %d bytes)", ErrTooLarge, maxBytes)
	}
	return data, nil
}
