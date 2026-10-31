package notion

import "testing"

func TestMatches(t *testing.T) {
	a := &Adapter{}
	cases := map[string]bool{
		"https://www.notion.so/Investment-Memo-0123456789abcdef0123456789abcdef":         true,
		"https://www.notion.so/acme/Roadmap-0123456789abcdef0123456789abcdef?pvs=4":      true,
		"https://notion.so/0123456789abcdef0123456789abcdef":                             true,
		"https://acme.notion.site/FAQ-fedcba9876543210fedcba9876543210":                  true,
		"https://acme.notion.site/":                                                      true,
		"https://app.notion.com/p/Investment-Memo-Acme-0123456789abcdef0123456789abcdef": true,
		"https://www.notion.com/help/public-pages-and-web-publishing":                    false,
		"https://www.notion.com/templates/investment-memo":                               false,
		"https://docsend.com/view/abc123":                                                false,
		"https://example.com/0123456789abcdef0123456789abcdef":                           false,
	}
	for u, want := range cases {
		if got := a.Matches(u); got != want {
			t.Errorf("Matches(%q) = %v, want %v", u, got, want)
		}
	}
}
