package googledrive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/holman/deckk/internal/adapter"
	"github.com/holman/deckk/internal/adapter/googleslides"
)

func init() {
	adapter.Register(&Adapter{})
}

// Adapter handles files that live in Google Drive — most commonly a PDF a
// founder uploaded and shared via "anyone with the link". Unlike Google
// Slides, Drive doesn't convert anything: it hands back whatever bytes were
// uploaded, so this adapter only succeeds for files that are already PDFs
// (or native Slides decks, which it forwards to the Slides export).
type Adapter struct{}

func (a *Adapter) Name() string { return "google-drive" }

func (a *Adapter) Matches(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.ToLower(u.Host) == "drive.google.com" && fileID(u) != ""
}

// fileID pulls the Drive file ID out of the URL shapes Drive hands out:
//
//	drive.google.com/file/d/<id>/view
//	drive.google.com/file/d/<id>/edit
//	drive.google.com/open?id=<id>
//	drive.google.com/uc?id=<id>
//
// Returns "" when the URL isn't one of those.
func fileID(u *url.URL) string {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "file" && parts[1] == "d" && parts[2] != "" {
		return parts[2]
	}
	if len(parts) == 1 && (parts[0] == "open" || parts[0] == "uc") {
		return u.Query().Get("id")
	}
	return ""
}

// Fetch satisfies adapter.Adapter, but Drive hands back a finished PDF, so
// the CLI uses FetchPDF instead.
func (a *Adapter) Fetch(ctx context.Context, rawURL string, opts adapter.Options) ([]adapter.Slide, error) {
	return nil, fmt.Errorf("google-drive produces a whole PDF; use FetchPDF")
}

// FetchPDF downloads the file through Drive's direct-download endpoint.
// The confirm=t parameter skips the "can't scan this file for viruses"
// interstitial Drive shows for large files. Files that require a Google
// sign-in bounce to accounts.google.com (or 403), which we surface as a
// clear error.
//
// If the bytes that come back aren't a PDF, the link may point at a native
// Google Slides deck living in Drive, so we retry through the Slides export
// endpoint before giving up.
func (a *Adapter) FetchPDF(ctx context.Context, rawURL string, opts adapter.Options) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	id := fileID(u)
	if id == "" {
		return nil, fmt.Errorf("could not find a Drive file ID in %q", rawURL)
	}

	data, filename, err := download(ctx, id)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(data, []byte("%PDF")) {
		return data, nil
	}

	// Not a PDF. If this is a native Slides deck, Slides' export endpoint
	// will happily render it for us.
	slidesURL := "https://docs.google.com/presentation/d/" + id + "/export/pdf"
	if pdf, err := (&googleslides.Adapter{}).FetchPDF(ctx, slidesURL, opts); err == nil {
		return pdf, nil
	}

	return nil, fmt.Errorf("%s is not a PDF%s; deckk can only fetch PDFs (or native Slides decks) from Drive", describe(filename), kindHint(data))
}

// download fetches the raw file bytes and, when Drive supplies one, the
// original filename from Content-Disposition.
func download(ctx context.Context, id string) ([]byte, string, error) {
	dl := "https://drive.usercontent.google.com/download?" + url.Values{
		"id":      {id},
		"export":  {"download"},
		"confirm": {"t"},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dl, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.Request != nil && strings.HasSuffix(resp.Request.URL.Host, "accounts.google.com") {
		return nil, "", fmt.Errorf("this file requires a Google sign-in; only files shared as \"anyone with the link\" can be downloaded")
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusUnauthorized:
		return nil, "", fmt.Errorf("Drive refused the download (%s); is the file shared as \"anyone with the link\"?", resp.Status)
	case http.StatusNotFound:
		return nil, "", fmt.Errorf("Drive returned 404; the file may have been deleted or the link is wrong")
	default:
		return nil, "", fmt.Errorf("download returned %s", resp.Status)
	}

	var filename string
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		filename = params["filename"]
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read download: %w", err)
	}
	return data, filename, nil
}

func describe(filename string) string {
	if filename == "" {
		return "the Drive file"
	}
	return fmt.Sprintf("%q", filename)
}

// kindHint guesses at what the non-PDF bytes are so the error can point the
// user in a useful direction.
func kindHint(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("PK")):
		return " (it looks like an Office file, e.g. .pptx — export it to PDF first)"
	case bytes.Contains(data[:min(len(data), 512)], []byte("<html")), bytes.Contains(data[:min(len(data), 512)], []byte("<!DOCTYPE")):
		return " (Drive returned a web page instead of the file; it may not be shared as \"anyone with the link\")"
	}
	return ""
}
