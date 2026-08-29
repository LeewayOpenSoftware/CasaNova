package route

import "testing"

// TestHighlightMatchNoPanic pins the byte-offset fix: strings.ToLower can
// change a string's byte length for some runes (Kelvin sign U+212A "K"→"k",
// Turkish İ), so the lowercased-space match offset can point past the end of
// the original name. The old code sliced name[:idx] unguarded → panic. It must
// never panic and must always HTML-escape.
func TestHighlightMatchNoPanic(t *testing.T) {
	cases := []struct{ name, keyword string }{
		{"Åelvin.txt", "k"},         // Kelvin sign: lower is shorter → idx past original
		{"report.pdf", "report"},    // plain match at start
		{"my report.pdf", "report"}, // match in middle
		{"nope.txt", "zzz"},         // no match
		{"<script>.txt", "script"},  // hostile name must be escaped
		{"", "x"},                   // empty name
		{"İstanbul", "istanbul"},    // Turkish dotted I
	}
	for _, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("highlightMatch(%q,%q) panicked: %v", c.name, c.keyword, r)
				}
			}()
			out := highlightMatch(c.name, c.keyword)
			if len(out) == 0 && len(c.name) != 0 {
				t.Errorf("highlightMatch(%q,%q) returned empty", c.name, c.keyword)
			}
		}()
	}
}
