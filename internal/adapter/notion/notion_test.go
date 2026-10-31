package notion

import "testing"

func TestMatches(t *testing.T) {
	a := &Adapter{}
	cases := map[string]bool{
		"https://www.notion.so/Investment-Memo-2f8d069f7c4080028e88da81dddb14da":               true,
		"https://www.notion.so/acme/Roadmap-2f8d069f7c4080028e88da81dddb14da?pvs=4":            true,
		"https://notion.so/2f8d069f7c4080028e88da81dddb14da":                                   true,
		"https://acme.notion.site/FAQ-a0d5469588f245f2b95f2a0c21bc030b":                        true,
		"https://acme.notion.site/":                                                            true,
		"https://app.notion.com/p/Investment-Memo-BrightCore-2f8d069f7c4080028e88da81dddb14da": true,
		"https://www.notion.com/help/public-pages-and-web-publishing":                          false,
		"https://www.notion.com/templates/investment-memo":                                     false,
		"https://docsend.com/view/abc123":                                                      false,
		"https://example.com/2f8d069f7c4080028e88da81dddb14da":                                 false,
	}
	for u, want := range cases {
		if got := a.Matches(u); got != want {
			t.Errorf("Matches(%q) = %v, want %v", u, got, want)
		}
	}
}
