package pdfurl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/holman/deckk/internal/adapter"
	_ "github.com/holman/deckk/internal/adapter/googleslides"
)

func TestMatches(t *testing.T) {
	a := &Adapter{}
	cases := map[string]bool{
		"https://example.com/deck.pdf":         true,
		"http://example.com/files/123?sig=abc": true,
		"ftp://example.com/deck.pdf":           false,
		"deck.pdf":                             false,
		"":                                     false,
	}
	for raw, want := range cases {
		if got := a.Matches(raw); got != want {
			t.Errorf("Matches(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestFetchPDF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/deck.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write([]byte("%PDF-1.7\n%fake"))
		case "/redirect":
			http.Redirect(w, r, "/deck.pdf", http.StatusFound)
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>nope</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := &Adapter{}
	ctx := context.Background()

	for _, path := range []string{"/deck.pdf", "/redirect"} {
		data, err := a.FetchPDF(ctx, srv.URL+path, adapter.Options{})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !strings.HasPrefix(string(data), "%PDF") {
			t.Fatalf("%s: got non-PDF bytes %q", path, data)
		}
	}

	if _, err := a.FetchPDF(ctx, srv.URL+"/page", adapter.Options{}); err == nil || !strings.Contains(err.Error(), "no deckk adapter") {
		t.Fatalf("html: expected no-adapter error, got %v", err)
	}
	if _, err := a.FetchPDF(ctx, srv.URL+"/missing", adapter.Options{}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404: expected status error, got %v", err)
	}
}

func TestFallbackOrdering(t *testing.T) {
	if a := adapter.Find("https://docs.google.com/presentation/d/abc/edit"); a != nil && a.Name() == "pdf" {
		t.Fatal("pdf fallback should not win over a site-specific adapter")
	}
	if a := adapter.Find("https://example.com/deck.pdf"); a == nil || a.Name() != "pdf" {
		t.Fatalf("expected pdf fallback, got %v", a)
	}
}
