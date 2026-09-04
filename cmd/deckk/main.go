package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/holman/deckk/internal/adapter"
	_ "github.com/holman/deckk/internal/adapter/canva"
	_ "github.com/holman/deckk/internal/adapter/docsend"
	_ "github.com/holman/deckk/internal/adapter/googleslides"
	_ "github.com/holman/deckk/internal/adapter/pitch"
	"github.com/holman/deckk/internal/pdf"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		output      string
		headful     bool
		email       string
		timeout     time.Duration
		showVersion bool
	)
	flag.StringVar(&output, "o", defaultOutput(), "output PDF path")
	flag.StringVar(&output, "output", defaultOutput(), "output PDF path")
	flag.BoolVar(&headful, "headful", false, "show the browser window (debugging)")
	flag.StringVar(&email, "email", "", "email to use for email-gated decks (defaults to git user.email)")
	flag.DurationVar(&timeout, "timeout", 5*time.Minute, "total timeout")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: deckk [flags] <url>\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if showVersion {
		fmt.Println(version())
		return nil
	}

	if flag.NArg() != 1 {
		flag.Usage()
		return fmt.Errorf("expected exactly one URL argument")
	}
	rawURL := flag.Arg(0)

	a := adapter.Find(rawURL)
	if a == nil {
		return fmt.Errorf("no adapter matched %q (registered: %s)", rawURL, adapterNames())
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if email == "" {
		if gitEmail, ok := gitUserEmail(); ok {
			email = gitEmail
			fmt.Fprintf(os.Stderr, "using git user.email (%s) for any email gate\n", email)
		}
	}

	fmt.Fprintf(os.Stderr, "Using %s adapter…\n", a.Name())
	opts := adapter.Options{Headful: headful, Email: email}

	// Some sources (e.g. Google Slides) expose a ready-made PDF export, so
	// there's nothing to screenshot or stitch — just save the bytes.
	var pdfBytes []byte
	if pf, ok := a.(adapter.PDFFetcher); ok {
		data, err := pf.FetchPDF(ctx, rawURL, opts)
		if err != nil {
			return err
		}
		pdfBytes = data
		fmt.Fprintf(os.Stderr, "Fetched deck PDF directly. Writing…\n")
	}

	var slides []adapter.Slide
	if pdfBytes == nil {
		var err error
		slides, err = a.Fetch(ctx, rawURL, opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Fetched %d slide(s). Writing PDF…\n", len(slides))
	}

	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	f, err := os.Create(output)
	if err != nil {
		return err
	}
	defer f.Close()

	if pdfBytes != nil {
		if _, err := f.Write(pdfBytes); err != nil {
			return err
		}
	} else if err := pdf.Write(f, slides); err != nil {
		return err
	}

	fmt.Printf("PDF saved to %s\n", output)
	return nil
}

// gitUserEmail returns the configured git user.email, if any. Used as a
// reasonable default for email-gated decks so users rarely have to pass
// --email by hand.
func gitUserEmail() (string, bool) {
	out, err := exec.Command("git", "config", "--get", "user.email").Output()
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", false
	}
	return s, true
}

func defaultOutput() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "deck.pdf"
	}
	return filepath.Join(home, "Desktop", "deck.pdf")
}

// version reports the module version baked into the binary by `go install`
// or `go build` of a tagged module. Returns "dev" when no version is
// available (e.g. plain `go build` inside the repo).
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	return info.Main.Version
}

func adapterNames() string {
	names := ""
	for i, a := range adapter.All() {
		if i > 0 {
			names += ", "
		}
		names += a.Name()
	}
	if names == "" {
		return "(none)"
	}
	return names
}
