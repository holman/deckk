package pdf

import (
	"bytes"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"

	"github.com/go-pdf/fpdf"

	"github.com/holman/deckk/internal/adapter"
)

// Write stitches slides into a PDF, one slide per page, with each page sized
// to the slide image's own aspect ratio so there's no letterboxing.
func Write(w io.Writer, slides []adapter.Slide) error {
	if len(slides) == 0 {
		return fmt.Errorf("no slides to write")
	}

	pdf := fpdf.New("L", "pt", "A4", "")
	pdf.SetAutoPageBreak(false, 0)

	for i, s := range slides {
		cfg, format, err := image.DecodeConfig(bytes.NewReader(s.Data))
		if err != nil {
			return fmt.Errorf("decode slide %d (content-type %q): %w", i+1, s.ContentType, err)
		}

		// Use the image's pixel dimensions as the page size (in points). This
		// gives us a 1:1 aspect-ratio match without scaling artifacts.
		w, h := float64(cfg.Width), float64(cfg.Height)
		// fpdf transposes Wd/Ht when orientation is "L"; always use "P" so the
// dimensions we pass in match the page that comes out.
pdf.AddPageFormat("P", fpdf.SizeType{Wd: w, Ht: h})

		imgType := fpdfImageType(format)
		name := fmt.Sprintf("slide-%d.%s", i+1, imgType)
		pdf.RegisterImageOptionsReader(name, fpdf.ImageOptions{ImageType: imgType}, bytes.NewReader(s.Data))
		pdf.ImageOptions(name, 0, 0, w, h, false, fpdf.ImageOptions{ImageType: imgType}, 0, "")
	}

	return pdf.Output(w)
}

// fpdfImageType maps Go's image package format string ("png", "jpeg") to
// the strings fpdf expects ("png", "jpg").
func fpdfImageType(format string) string {
	if format == "jpeg" {
		return "jpg"
	}
	return format
}
