//go:build playwright

package browser

import (
	"context"
	"strings"
	"testing"

	"github.com/yedamao/wally/internal/config"
	"github.com/yedamao/wally/internal/security"
)

func TestPlaywrightSmoke(t *testing.T) {
	cfg := config.Default().Tools.Browser
	b, err := newPlaywrightBackend(Config{Browser: cfg, Policy: &security.Policy{}})
	if err != nil {
		t.Skipf("Playwright Chromium is not installed: %v", err)
	}
	if _, err := b.Snapshot(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.URL(), "about:blank") {
		t.Fatalf("URL = %q", b.URL())
	}
}
