package index

import "testing"

func TestMatcherFindFirst(t *testing.T) {
	cases := []struct {
		pattern       string
		fixed, nocase bool
		line          string
		want          []int // nil = no match
	}{
		{"needle", false, false, "a needle here", []int{2, 8}},
		{"needle", false, false, "no match", nil},
		{"NeEdLe", false, true, "a needle here", []int{2, 8}},
		{"a+b", false, false, "xx aaab yy", []int{3, 7}},
		{"a+b", true, false, "literal a+b here", []int{8, 11}},
		{"需要", false, false, "中文需要匹配", []int{6, 12}},
	}
	for _, c := range cases {
		m, err := MatcherFor(c.pattern, c.fixed, c.nocase)
		if err != nil {
			t.Fatalf("MatcherFor(%q): %v", c.pattern, err)
		}
		got := m.FindFirst([]byte(c.line))
		if (got == nil) != (c.want == nil) || got != nil && (got[0] != c.want[0] || got[1] != c.want[1]) {
			t.Errorf("FindFirst(%q, %q) = %v, want %v", c.pattern, c.line, got, c.want)
		}
	}
	// the literal fast path must engage for plain needles
	if m, _ := MatcherFor("plain", false, false); m.PlainLit != "plain" {
		t.Error("plain literal did not take the fast path")
	}
	if m, _ := MatcherFor("plain", false, true); m.PlainLit != "plain" || !m.Fold {
		t.Error("ASCII nocase literal did not take the folded fast path")
	}
	if m, _ := MatcherFor("需要", false, true); m.PlainLit != "" {
		t.Error("non-ASCII nocase literal must use the regex engine")
	}
}
