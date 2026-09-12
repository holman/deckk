package pdfurl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/holman/deckk/internal/adapter"
)

func init() {
	adapter.RegisterFallback(&Adapter{})
}

// Adapter is the base case: a URL that just serves a PDF. It's registered
// as the fallback, so it only sees URLs no site-specific adapter claimed.
// It matches on scheme alone rather than a ".pdf" suffix because plenty of
// direct links (CDNs, signed URLs, attachment endpoints) don't end in .pdf;
// FetchPDF verifies the bytes instead.
type Adapter struct{}

func (a *Adapter) Name() string { return "pdf" }

func (a *Adapter) Matches(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return (scheme == "http" || scheme == "https") && u.Host != ""
}

// Fetch satisfies adapter.Adapter, but the source is already a PDF, so the
// CLI uses FetchPDF instead.
func (a *Adapter) Fetch(ctx context.Context, rawURL string, opts adapter.Options) ([]adapter.Slide, error) {
	return nil, fmt.Errorf("pdf produces a whole PDF; use FetchPDF")
}

// FetchPDF downloads the URL and returns the bytes if they're a PDF. Since
// this adapter only runs for sites deckk doesn't otherwise know, a non-PDF
// response is reported as "no adapter for this site" rather than as a
// download failure.
func (a *Adapter) FetchPDF(ctx context.Context, rawURL string, opts adapter.Options) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/pdf,*/*;q=0.8")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read download: %w", err)
	}
	if !bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"), []byte("%PDF")) {
		host := "this site"
		if resp.Request != nil && resp.Request.URL != nil {
			host = resp.Request.URL.Host
		}
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "unknown"
		}
		return nil, fmt.Errorf("no deckk adapter for %s, and the URL didn't serve a PDF (content-type %s)", host, ct)
	}
	return data, nil
}
