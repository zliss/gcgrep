package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func matcher(t *testing.T, gitignore string) *Matcher {
	t.Helper()
	dir := t.TempDir()
	if gitignore != "" {
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignore), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Load(dir)
}

func TestGitAlwaysIgnored(t *testing.T) {
	m := matcher(t, "")
	for _, p := range []string{".git", ".git/config", "sub/.git/HEAD"} {
		if !m.Ignored(p, false) && !m.Ignored(p, true) {
			t.Errorf("%s should be ignored", p)
		}
	}
	if m.Ignored("github.go", false) {
		t.Error("github.go wrongly ignored")
	}
}

func TestGitignorePatterns(t *testing.T) {
	m := matcher(t, `
node_modules/
*.log
/build
dist/**
docs/*.tmp
`)
	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"node_modules", true, true},
		{"sub/node_modules", true, true},
		{"node_modules/x/y.js", false, true},
		{"app.log", false, true},
		{"deep/in/tree.log", false, true},
		{"build", true, true},
		{"build/out.txt", false, true},
		{"sub/build", true, false}, // anchored by leading /
		{"dist/bundle.js", false, true},
		{"docs/a.tmp", false, true},
		{"docs/sub/a.tmp", false, false}, // single * does not cross /
		{"main.go", false, false},
		{"logger.go", false, false}, // *.log must not match substring
	}
	for _, c := range cases {
		if got := m.Ignored(c.path, c.isDir); got != c.want {
			t.Errorf("Ignored(%q, dir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

func TestDirOnlyDoesNotMatchFile(t *testing.T) {
	m := matcher(t, "cache/\n")
	if m.Ignored("cache", false) {
		t.Error("dir-only pattern matched a plain file")
	}
	if !m.Ignored("cache", true) {
		t.Error("dir-only pattern missed the directory")
	}
	if !m.Ignored("cache/data.bin", false) {
		t.Error("contents of ignored dir not ignored")
	}
}
