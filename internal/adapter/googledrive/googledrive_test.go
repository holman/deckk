package googledrive

import "testing"

func TestMatches(t *testing.T) {
	a := &Adapter{}
	cases := map[string]bool{
		"https://drive.google.com/file/d/abc123XYZ/view":             true,
		"https://drive.google.com/file/d/abc123XYZ/view?usp=sharing": true,
		"https://drive.google.com/file/d/abc123XYZ/edit":             true,
		"https://drive.google.com/file/d/abc123XYZ":                  true,
		"https://drive.google.com/open?id=abc123XYZ":                 true,
		"https://drive.google.com/uc?id=abc123XYZ&export=download":   true,
		"https://drive.google.com/drive/folders/abc123XYZ":           false,
		"https://drive.google.com/file/d/":                           false,
		"https://drive.google.com/open":                              false,
		"https://docs.google.com/presentation/d/abc123XYZ/edit":      false,
		"https://example.com/file/d/abc123XYZ/view":                  false,
	}
	for raw, want := range cases {
		if got := a.Matches(raw); got != want {
			t.Errorf("Matches(%q) = %v, want %v", raw, got, want)
		}
	}
}
