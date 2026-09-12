package adapter

import "context"

// Slide is one page of source material — typically an image extracted from
// the source site. The PDF stitcher decides how to lay it out.
type Slide struct {
	Data        []byte
	ContentType string // e.g. "image/png", "image/jpeg"
}

// Adapter knows how to turn a URL on a specific site into an ordered list of
// slides. Each site (docsend, future: pitch.com, gamma, etc.) gets its own.
type Adapter interface {
	Name() string
	Matches(rawURL string) bool
	Fetch(ctx context.Context, rawURL string, opts Options) ([]Slide, error)
}

// PDFFetcher is an optional interface for adapters whose source can hand
// over the whole deck as a ready-made PDF (e.g. an export endpoint), so
// there's no need to screenshot slide-by-slide. When an adapter implements
// it, the CLI calls FetchPDF instead of Fetch and writes the bytes straight
// to the output file.
type PDFFetcher interface {
	FetchPDF(ctx context.Context, rawURL string, opts Options) ([]byte, error)
}

// Options carries CLI-level toggles that an adapter may need to honor.
type Options struct {
	Headful bool
	// Email is used to satisfy email-gated viewers (e.g. docsend prompting
	// the visitor before showing the deck). May be empty; if it's empty
	// and a gate is detected, the adapter should return a clear error.
	Email string
}

var (
	registry []Adapter
	fallback Adapter
)

func Register(a Adapter) { registry = append(registry, a) }

// RegisterFallback installs the adapter Find consults only after every
// site-specific adapter has declined the URL. Package init order isn't
// something we want to lean on for "try this last", so it's explicit.
func RegisterFallback(a Adapter) { fallback = a }

// Find returns the first registered adapter that claims the URL, then the
// fallback adapter if it claims the URL, or nil.
func Find(rawURL string) Adapter {
	for _, a := range registry {
		if a.Matches(rawURL) {
			return a
		}
	}
	if fallback != nil && fallback.Matches(rawURL) {
		return fallback
	}
	return nil
}

// All returns every registered adapter, fallback last (used for --list
// output).
func All() []Adapter {
	if fallback == nil {
		return registry
	}
	return append(append([]Adapter{}, registry...), fallback)
}
