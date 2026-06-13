// Package ignore implements exclusion matching for indexing. By design
// gcgrep indexes EVERYTHING under a root except .git: hidden ignore
// semantics cost users trust ("why is my file not found?"), so exclusions
// are explicit only — a .gcgrepignore file at the root, written by the
// user, with gitignore-style syntax: *, ?, **, anchored (leading /) and
// directory-only (trailing /) patterns. Negation (!) lines are skipped.
package ignore

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type rule struct {
	exact   *regexp.Regexp // pattern matches the whole rel path
	prefix  *regexp.Regexp // pattern matches an ancestor directory
	dirOnly bool
}

type Matcher struct {
	rules []rule
}

// ControlFile is the per-root exclusion file users may create.
const ControlFile = ".gcgrepignore"

// Load reads root/.gcgrepignore; a missing file yields a matcher with
// only the built-in .git rule (i.e. index everything).
func Load(root string) *Matcher {
	m := &Matcher{}
	f, err := os.Open(filepath.Join(root, ControlFile))
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if r, ok := compile(line); ok {
			m.rules = append(m.rules, r)
		}
	}
	return m
}

// compile translates one gitignore pattern into regexps over the
// slash-separated path relative to root.
func compile(pat string) (rule, bool) {
	var r rule
	if strings.HasSuffix(pat, "/") {
		r.dirOnly = true
		pat = strings.TrimSuffix(pat, "/")
	}
	// a pattern containing a slash is anchored to the root, per gitignore
	anchored := strings.HasPrefix(pat, "/") || strings.Contains(pat, "/")
	pat = strings.TrimPrefix(pat, "/")

	var b strings.Builder
	if anchored {
		b.WriteString("^")
	} else {
		b.WriteString(`(?:^|.*/)`)
	}
	i := 0
	for i < len(pat) {
		switch {
		case strings.HasPrefix(pat[i:], "**/"):
			b.WriteString(`(?:.*/)?`)
			i += 3
		case strings.HasPrefix(pat[i:], "**"):
			b.WriteString(`.*`)
			i += 2
		case pat[i] == '*':
			b.WriteString(`[^/]*`)
			i++
		case pat[i] == '?':
			b.WriteString(`[^/]`)
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(pat[i])))
			i++
		}
	}
	body := b.String()
	exact, err1 := regexp.Compile(body + `$`)
	prefix, err2 := regexp.Compile(body + `/`)
	if err1 != nil || err2 != nil {
		return rule{}, false
	}
	r.exact, r.prefix = exact, prefix
	return r, true
}

// Ignored reports whether the slash-separated relative path should be
// skipped. isDir distinguishes directory-only rules.
func (m *Matcher) Ignored(rel string, isDir bool) bool {
	if rel == ".git" || strings.HasPrefix(rel, ".git/") || strings.Contains(rel, "/.git/") || strings.HasSuffix(rel, "/.git") {
		return true
	}
	for _, r := range m.rules {
		// an ancestor directory matched the pattern: always ignored,
		// because that ancestor is necessarily a directory
		if r.prefix.MatchString(rel) {
			return true
		}
		if r.exact.MatchString(rel) && (isDir || !r.dirOnly) {
			return true
		}
	}
	return false
}
