package docsend

import "testing"

func TestIsSpaceURL(t *testing.T) {
	cases := map[string]bool{
		"https://docsend.com/view/s/abc123":        true,
		"https://docsend.com/view/s/abc123?x=1":    true,
		"https://docsend.com/view/abc123":          false,
		"https://docsend.com/view/abc123/d/def456": false,
		"https://www.docsend.com/view/sabc123":     false,
		"https://docsend.com/v/abc123/deck":        false,
		"https://docsend.com/view/s":               false,
	}
	for raw, want := range cases {
		if got := isSpaceURL(raw); got != want {
			t.Errorf("isSpaceURL(%q) = %v, want %v", raw, got, want)
		}
	}
}
