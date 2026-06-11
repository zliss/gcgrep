package index

import (
	"fmt"
	"regexp"
	"testing"
)

func add(ix *Index, path, content string) {
	ix.Add(FileMeta{Path: path, Size: int64(len(content)), MtimeNS: 1}, []byte(content))
}

func search(ix *Index, pattern string, nocase bool) []Match {
	pat := pattern
	if nocase {
		pat = "(?i)" + pat
	}
	re := regexp.MustCompile(pat)
	return ix.Search(re, SearchOpts{Literal: ExtractLiteral(pattern, false)}).Matches
}

func TestBasicSearch(t *testing.T) {
	ix := New("/r")
	add(ix, "a.go", "package main\nfunc HelloWorld() {}\n")
	add(ix, "b.go", "package main\nvar x = 1\n")

	m := search(ix, "HelloWorld", false)
	if len(m) != 1 || m[0].Path != "a.go" || m[0].Line != 2 || m[0].Text != "func HelloWorld() {}" {
		t.Fatalf("unexpected matches: %+v", m)
	}
	if got := search(ix, "NoSuchThing", false); len(got) != 0 {
		t.Fatalf("expected no matches, got %+v", got)
	}
}

func TestCaseInsensitive(t *testing.T) {
	ix := New("/r")
	add(ix, "a.go", "const FOOBARBAZ = 1\n")
	if m := search(ix, "foobarbaz", true); len(m) != 1 {
		t.Fatalf("nocase search failed: %+v", m)
	}
	if m := search(ix, "foobarbaz", false); len(m) != 0 {
		t.Fatalf("case-sensitive search should not match: %+v", m)
	}
}

func TestRegexSearch(t *testing.T) {
	ix := New("/r")
	add(ix, "a.go", "func GetUser() {}\nfunc GetOrder() {}\nfunc SetUser() {}\n")
	m := search(ix, `func Get\w+\(`, false)
	if len(m) != 2 {
		t.Fatalf("expected 2 matches, got %+v", m)
	}
}

func TestUpdateAndRemove(t *testing.T) {
	ix := New("/r")
	add(ix, "a.go", "alpha\n")
	add(ix, "a.go", "bravo\n") // update replaces
	if m := search(ix, "alpha", false); len(m) != 0 {
		t.Fatalf("stale content still matches: %+v", m)
	}
	if m := search(ix, "bravo", false); len(m) != 1 {
		t.Fatalf("new content not found: %+v", m)
	}
	ix.Remove("a.go")
	if m := search(ix, "bravo", false); len(m) != 0 {
		t.Fatalf("removed file still matches: %+v", m)
	}
}

func TestRemovePrefix(t *testing.T) {
	ix := New("/r")
	add(ix, "sub/a.go", "needleone\n")
	add(ix, "sub/deep/b.go", "needleone\n")
	add(ix, "other/c.go", "needleone\n")
	ix.RemovePrefix("sub")
	m := search(ix, "needleone", false)
	if len(m) != 1 || m[0].Path != "other/c.go" {
		t.Fatalf("RemovePrefix wrong result: %+v", m)
	}
}

func TestCompaction(t *testing.T) {
	ix := New("/r")
	for i := 0; i < 200; i++ {
		add(ix, fmt.Sprintf("f%d.go", i), fmt.Sprintf("token%d unique\n", i))
	}
	for i := 0; i < 150; i++ {
		ix.Remove(fmt.Sprintf("f%d.go", i))
	}
	if ix.NumFiles() != 50 {
		t.Fatalf("NumFiles=%d", ix.NumFiles())
	}
	if m := search(ix, "token199", false); len(m) != 1 {
		t.Fatalf("post-compaction search broken: %+v", m)
	}
	if m := search(ix, "token0 ", false); len(m) != 0 {
		t.Fatalf("removed file matches after compaction: %+v", m)
	}
}

func TestSearchLimit(t *testing.T) {
	ix := New("/r")
	add(ix, "a.txt", "hit\nhit\nhit\nhit\n")
	re := regexp.MustCompile("hit")
	res := ix.Search(re, SearchOpts{Literal: "hit", Limit: 2})
	if len(res.Matches) != 2 || !res.Truncated {
		t.Fatalf("limit not applied: %d matches truncated=%v", len(res.Matches), res.Truncated)
	}
}

func TestExtractLiteral(t *testing.T) {
	cases := []struct {
		pattern string
		want    string
	}{
		{"HelloWorld", "helloworld"},
		{`func Get\w+\(`, "func get"},
		{"a|b", ""},                   // alternation: no required literal
		{"x?", ""},                    // optional: nothing required
		{`(prefix)+suffix`, "prefix"}, // plus guarantees one occurrence
		{"ab", ""},                    // too short for trigrams
		{`foo(bar|baz)`, "foo"},
	}
	for _, c := range cases {
		if got := ExtractLiteral(c.pattern, false); got != c.want {
			t.Errorf("ExtractLiteral(%q) = %q, want %q", c.pattern, got, c.want)
		}
	}
	if got := ExtractLiteral("a.b*c", true); got != "a.b*c" {
		t.Errorf("fixed literal mangled: %q", got)
	}
}

func TestPlainLiteralFastPath(t *testing.T) {
	ix := New("/r")
	add(ix, "a.go", "Foo LeaderElection bar\nleaderelection lower\nLEADERELECTION upper\n")
	re := regexp.MustCompile("(?i)leaderelection")
	res := ix.Search(re, SearchOpts{Literal: "leaderelection", PlainLiteral: "leaderelection", FoldCase: true})
	if len(res.Matches) != 3 {
		t.Fatalf("fold fast path: got %d matches, want 3: %+v", len(res.Matches), res.Matches)
	}
	res = ix.Search(regexp.MustCompile("LeaderElection"), SearchOpts{Literal: "leaderelection", PlainLiteral: "LeaderElection"})
	if len(res.Matches) != 1 {
		t.Fatalf("case-sensitive fast path: got %d, want 1", len(res.Matches))
	}
	if lit, ok := PlainLiteral("Get.*User", false); ok {
		t.Fatalf("regex wrongly classified as literal: %q", lit)
	}
	if lit, ok := PlainLiteral("a.b", true); !ok || lit != "a.b" {
		t.Fatalf("fixed pattern not literal: %q %v", lit, ok)
	}
}
