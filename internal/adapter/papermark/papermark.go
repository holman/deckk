package papermark

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/holman/deckk/internal/adapter"
)

func init() {
	adapter.Register(&Adapter{})
}

// Adapter handles papermark.com/view/... links.
//
// Papermark's viewer is a React app that, on mount, POSTs to /api/views and
// gets back a JSON blob describing the deck: a view id plus one entry per
// page, each with a signed image URL (for PDFs that Papermark has already
// rasterised). Only the first ~10 pages come with URLs; the rest are signed
// on demand via /api/views/pages. Rather than paging through the viewer and
// screenshotting, we load the page once, catch that /api/views response off
// the wire, ask for the remaining page URLs in a couple of batches, and
// download the page images directly. Papermark records a single view, and
// only page 1 gets a "viewed" event — about the lightest touch possible.
type Adapter struct{}

func (a *Adapter) Name() string { return "papermark" }

func (a *Adapter) Matches(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	if host != "papermark.com" && !strings.HasSuffix(host, ".papermark.com") {
		return false
	}
	return strings.HasPrefix(u.Path, "/view/")
}

// viewsResponse is the subset of Papermark's POST /api/views response we
// care about.
type viewsResponse struct {
	Type     string      `json:"type"` // "email-verification" when the link needs an OTP
	Message  string      `json:"message"`
	ViewID   string      `json:"viewId"`
	File     string      `json:"file"`
	FileType string      `json:"fileType"`
	Pages    []pageEntry `json:"pages"`
}

type pageEntry struct {
	File       *string     `json:"file"`
	PageNumber json.Number `json:"pageNumber"`
}

func (p pageEntry) num() int {
	n, _ := strconv.Atoi(p.PageNumber.String())
	return n
}

// viewsRequest is what the viewer sends to /api/views; we need the
// document version id to ask for the lazily-signed pages.
type viewsRequest struct {
	DocumentVersionID string `json:"documentVersionId"`
	LinkID            string `json:"linkId"`
}

// apiCall is one captured POST /api/views exchange.
type apiCall struct {
	Req  viewsRequest
	Resp viewsResponse
	Raw  []byte
	Err  error
}

// Papermark signs at most this many page URLs per /api/views/pages call.
const maxPagesPerRequest = 15

func (a *Adapter) Fetch(ctx context.Context, rawURL string, opts adapter.Options) ([]adapter.Slide, error) {
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	if opts.Headful {
		allocOpts = append(allocOpts, chromedp.Flag("headless", false))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, allocOpts...)
	defer cancelAlloc()

	bctx, cancelB := chromedp.NewContext(allocCtx)
	defer cancelB()

	calls := make(chan apiCall, 4)
	listenForViewsAPI(bctx, calls)

	if err := chromedp.Run(bctx,
		network.Enable(),
		chromedp.EmulateViewport(1600, 1200),
		chromedp.Navigate(rawURL),
	); err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}

	// An unprotected link fires /api/views on mount. A protected one shows
	// the access form first, so if nothing arrives quickly, look for a gate.
	call, ok := waitForCall(bctx, calls, 8*time.Second)
	if !ok {
		if err := handleAccessForm(bctx, opts.Email); err != nil {
			return nil, err
		}
		call, ok = waitForCall(bctx, calls, 20*time.Second)
		if !ok {
			return nil, fmt.Errorf("viewer never requested the deck (no /api/views response)")
		}
	}
	if call.Err != nil {
		return nil, call.Err
	}
	resp := call.Resp

	switch {
	case resp.Type == "email-verification":
		return nil, fmt.Errorf("this link requires email verification (a one-time code sent to %s) — deckk can't complete that automatically", opts.Email)
	case len(resp.Pages) == 0 && resp.File != "":
		return nil, fmt.Errorf("this deck is served as a raw %s file rather than page images; not supported yet", orUnknown(resp.FileType))
	case len(resp.Pages) == 0:
		msg := resp.Message
		if msg == "" {
			msg = string(truncate(call.Raw, 300))
		}
		return nil, fmt.Errorf("viewer returned no pages: %s", msg)
	}

	pages := resp.Pages
	sort.Slice(pages, func(i, j int) bool { return pages[i].num() < pages[j].num() })
	fmt.Fprintf(os.Stderr, "deck has %d page(s)\n", len(pages))

	// Ask for URLs for any pages the initial response left unsigned.
	var missing []int
	for _, p := range pages {
		if p.File == nil || *p.File == "" {
			missing = append(missing, p.num())
		}
	}
	if len(missing) > 0 {
		if call.Req.DocumentVersionID == "" || resp.ViewID == "" {
			return nil, fmt.Errorf("%d page(s) need signing but the view/version ids weren't captured", len(missing))
		}
		fmt.Fprintf(os.Stderr, "requesting URLs for %d more page(s)…\n", len(missing))
		signed, err := fetchPageURLs(bctx, resp.ViewID, call.Req.DocumentVersionID, missing)
		if err != nil {
			return nil, err
		}
		for i := range pages {
			if u, ok := signed[pages[i].num()]; ok && (pages[i].File == nil || *pages[i].File == "") {
				u := u
				pages[i].File = &u
			}
		}
	}

	client := &http.Client{Timeout: 60 * time.Second}
	slides := make([]adapter.Slide, 0, len(pages))
	for _, p := range pages {
		if p.File == nil || *p.File == "" {
			return nil, fmt.Errorf("no URL for page %d", p.num())
		}
		data, ctype, err := download(ctx, client, *p.File)
		if err != nil {
			return nil, fmt.Errorf("download page %d: %w", p.num(), err)
		}
		if ctype != "image/png" && ctype != "image/jpeg" {
			return nil, fmt.Errorf("page %d is %s; only PNG/JPEG pages are supported", p.num(), ctype)
		}
		slides = append(slides, adapter.Slide{Data: data, ContentType: ctype})
	}

	if len(slides) == 0 {
		return nil, fmt.Errorf("no slides captured")
	}
	return slides, nil
}

// listenForViewsAPI watches network traffic for the viewer's POST /api/views
// and, once each one finishes, fetches its body and pushes the parsed
// exchange onto calls.
func listenForViewsAPI(bctx context.Context, calls chan<- apiCall) {
	var mu sync.Mutex
	pending := map[network.RequestID]viewsRequest{}

	chromedp.ListenTarget(bctx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventRequestWillBeSent:
			if e.Request == nil || e.Request.Method != "POST" {
				return
			}
			u, err := url.Parse(e.Request.URL)
			if err != nil || u.Path != "/api/views" {
				return
			}
			var req viewsRequest
			for _, entry := range e.Request.PostDataEntries {
				if b, err := base64.StdEncoding.DecodeString(entry.Bytes); err == nil {
					_ = json.Unmarshal(b, &req)
				}
			}
			mu.Lock()
			pending[e.RequestID] = req
			mu.Unlock()

		case *network.EventLoadingFinished:
			mu.Lock()
			req, ok := pending[e.RequestID]
			if ok {
				delete(pending, e.RequestID)
			}
			mu.Unlock()
			if !ok {
				return
			}
			// Don't block the event loop: fetch the body on its own goroutine.
			go func(id network.RequestID) {
				c := chromedp.FromContext(bctx)
				execCtx := cdp.WithExecutor(bctx, c.Target)
				body, err := network.GetResponseBody(id).Do(execCtx)
				call := apiCall{Req: req, Raw: body}
				if err != nil {
					call.Err = fmt.Errorf("read /api/views response: %w", err)
				} else if err := json.Unmarshal(body, &call.Resp); err != nil {
					call.Err = fmt.Errorf("parse /api/views response: %w (%s)", err, truncate(body, 200))
				}
				select {
				case calls <- call:
				case <-bctx.Done():
				}
			}(e.RequestID)

		case *network.EventLoadingFailed:
			mu.Lock()
			_, ok := pending[e.RequestID]
			if ok {
				delete(pending, e.RequestID)
			}
			mu.Unlock()
			if ok {
				select {
				case calls <- apiCall{Err: fmt.Errorf("/api/views request failed: %s", e.ErrorText)}:
				case <-bctx.Done():
				}
			}
		}
	})
}

func waitForCall(ctx context.Context, calls <-chan apiCall, d time.Duration) (apiCall, bool) {
	select {
	case c := <-calls:
		return c, true
	case <-time.After(d):
		return apiCall{}, false
	case <-ctx.Done():
		return apiCall{}, false
	}
}

// fetchPageURLsJS runs inside the page so the request rides on the viewer's
// own origin and cookies, exactly as the real client does when scrolling.
const fetchPageURLsJS = `
async (viewId, documentVersionId, pageNumbers) => {
  const r = await fetch('/api/views/pages', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ viewId, documentVersionId, pageNumbers }),
  });
  const text = await r.text();
  return { status: r.status, body: text };
}
`

type fetchResult struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

func fetchPageURLs(ctx context.Context, viewID, versionID string, nums []int) (map[int]string, error) {
	out := map[int]string{}
	for start := 0; start < len(nums); start += maxPagesPerRequest {
		end := start + maxPagesPerRequest
		if end > len(nums) {
			end = len(nums)
		}
		batch := nums[start:end]
		numsJSON, _ := json.Marshal(batch)
		script := fmt.Sprintf("(%s)(%q, %q, %s)", strings.TrimSpace(fetchPageURLsJS), viewID, versionID, numsJSON)

		var res fetchResult
		if err := chromedp.Run(ctx, chromedp.Evaluate(script, &res, func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true)
		})); err != nil {
			return nil, fmt.Errorf("request page URLs: %w", err)
		}
		if res.Status != 200 {
			return nil, fmt.Errorf("/api/views/pages returned %d: %s", res.Status, truncate([]byte(res.Body), 200))
		}
		var parsed struct {
			Pages []pageEntry `json:"pages"`
		}
		if err := json.Unmarshal([]byte(res.Body), &parsed); err != nil {
			return nil, fmt.Errorf("parse /api/views/pages: %w", err)
		}
		for _, p := range parsed.Pages {
			if p.File != nil && *p.File != "" {
				out[p.num()] = *p.File
			}
		}
	}
	return out, nil
}

func download(ctx context.Context, client *http.Client, u string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	// Trust the bytes over the header: signed-storage URLs sometimes come
	// back as application/octet-stream.
	ctype := http.DetectContentType(data)
	if i := strings.Index(ctype, ";"); i >= 0 {
		ctype = ctype[:i]
	}
	return data, ctype, nil
}

// --- access form (email / password gate) ---------------------------------

const findAccessFormJS = `
(() => {
  const visible = el => {
    if (!el) return false;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && getComputedStyle(el).visibility !== 'hidden';
  };
  const email = document.querySelector('input#email[type="email"], input[name="email"][type="email"]');
  const password = document.querySelector('input#password, input[name="password"]');
  return { email: visible(email), password: visible(password) };
})()
`

const fillAccessFormJS = `
(email) => {
  const input = document.querySelector('input#email[type="email"], input[name="email"][type="email"]');
  if (!input) return 'no email input';
  const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
  setter.call(input, email);
  input.dispatchEvent(new Event('input', { bubbles: true }));
  input.dispatchEvent(new Event('change', { bubbles: true }));
  const form = input.closest('form');
  if (form) {
    const btn = form.querySelector('button[type="submit"]') || form.querySelector('button');
    if (btn) { btn.click(); return 'submitted via form button'; }
    form.requestSubmit ? form.requestSubmit() : form.submit();
    return 'submitted via form.submit()';
  }
  return 'no form found';
}
`

type accessForm struct {
	Email    bool `json:"email"`
	Password bool `json:"password"`
}

// handleAccessForm fills Papermark's access form when the link is
// email-gated. Password-protected links are reported as an error since we
// have no way to supply one.
func handleAccessForm(ctx context.Context, email string) error {
	var gate accessForm
	if err := chromedp.Run(ctx, chromedp.Evaluate(findAccessFormJS, &gate)); err != nil {
		return fmt.Errorf("detect access form: %w", err)
	}
	if gate.Password {
		return fmt.Errorf("this deck is password-protected; deckk can't supply a passcode")
	}
	if !gate.Email {
		return fmt.Errorf("viewer never requested the deck and no access form was found (is the link still active?)")
	}
	if email == "" {
		return fmt.Errorf("this deck requires an email — re-run with --email=you@example.com (or set `git config --global user.email`)")
	}

	fmt.Fprintf(os.Stderr, "deck is email-gated; submitting %s…\n", email)
	var result string
	script := fmt.Sprintf("(%s)(%q)", strings.TrimSpace(fillAccessFormJS), email)
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &result)); err != nil {
		return fmt.Errorf("fill access form: %w", err)
	}
	fmt.Fprintf(os.Stderr, "gate %s\n", result)
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func truncate(b []byte, n int) []byte {
	if len(b) > n {
		return append(append([]byte{}, b[:n]...), []byte("…")...)
	}
	return b
}
