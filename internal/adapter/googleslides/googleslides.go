package googleslides

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
	adapter.Register(&Adapter{})
}

type Adapter struct{}

func (a *Adapter) Name() string { return "google-slides" }

func (a *Adapter) Matches(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.ToLower(u.Host) == "docs.google.com" && docPath(u.Path) != ""
}

// docPath returns the "d/<id>" (or "d/e/<id>" for published decks) portion
// of a Slides URL path, or "" if the path isn't a presentation link. Trailing
// segments like /edit, /view, or /present are ignored.
func docPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "presentation" || parts[1] != "d" || parts[2] == "" {
		return ""
	}
	if parts[2] == "e" {
		if len(parts) < 4 || parts[3] == "" {
			return ""
		}
		return "d/e/" + parts[3]
	}
	return "d/" + parts[2]
}

// Fetch satisfies adapter.Adapter, but Google Slides hands back a finished
// PDF via its export endpoint, so the CLI uses FetchPDF instead.
func (a *Adapter) Fetch(ctx context.Context, rawURL string, opts adapter.Options) ([]adapter.Slide, error) {
	return nil, fmt.Errorf("google-slides produces a whole PDF; use FetchPDF")
}

// FetchPDF downloads the deck through Slides' export endpoint
// (…/presentation/d/<id>/export/pdf) — the same PDF the in-browser
// File → Download produces. Works for any deck shared as "anyone with the
// link"; decks that require a Google sign-in bounce to accounts.google.com,
// which we surface as a clear error.
func (a *Adapter) FetchPDF(ctx context.Context, rawURL string, opts adapter.Options) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	dp := docPath(u.Path)
	if dp == "" {
		return nil, fmt.Errorf("could not find a presentation ID in %q", rawURL)
	}
	exportURL := "https://docs.google.com/presentation/" + dp + "/export/pdf"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, exportURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download export: %w", err)
	}
	defer resp.Body.Close()

	if resp.Request != nil && strings.HasSuffix(resp.Request.URL.Host, "accounts.google.com") {
		return nil, fmt.Errorf("this deck requires a Google sign-in; only decks shared as \"anyone with the link\" can be exported")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("export returned %s (is the deck shared as \"anyone with the link\"?)", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read export: %w", err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		return nil, fmt.Errorf("export did not return a PDF (the deck is probably not shared as \"anyone with the link\")")
	}
	return data, nil
}
