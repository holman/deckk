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

// Options carries CLI-level toggles that an adapter may need to honor.
type Options struct {
	Headful bool
	// Email is used to satisfy email-gated viewers (e.g. docsend prompting
	// the visitor before showing the deck). May be empty; if it's empty
	// and a gate is detected, the adapter should return a clear error.
	Email string
}

var registry []Adapter

func Register(a Adapter) { registry = append(registry, a) }

// Find returns the first registered adapter that claims the URL, or nil.
func Find(rawURL string) Adapter {
	for _, a := range registry {
		if a.Matches(rawURL) {
			return a
		}
	}
	return nil
}

// All returns every registered adapter (used for --list output).
func All() []Adapter { return registry }
