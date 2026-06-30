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
		if err := os.WriteFile(filepath.Join(dir, ".gcgrepignore"), []byte(gitignore), 0o600); err != nil {
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

func TestIgnorePatterns(t *testing.T) {
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

func TestAddPatterns(t *testing.T) {
	m := matcher(t, "")
	m.AddPatterns([]string{"vendor/", "*.min.js"})
	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"vendor", true, true},
		{"vendor/pkg/lib.go", false, true},
		{"sub/vendor", true, true},
		{"app.min.js", false, true},
		{"deep/bundle.min.js", false, true},
		{"app.js", false, false},
		{"main.go", false, false},
	}
	for _, c := range cases {
		if got := m.Ignored(c.path, c.isDir); got != c.want {
			t.Errorf("Ignored(%q, dir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

func TestAddPatternsComposesWithFile(t *testing.T) {
	m := matcher(t, "*.log\n")
	m.AddPatterns([]string{"*.tmp"})
	if !m.Ignored("app.log", false) {
		t.Error("file pattern missed")
	}
	if !m.Ignored("data.tmp", false) {
		t.Error("added pattern missed")
	}
	if m.Ignored("main.go", false) {
		t.Error("clean file wrongly ignored")
	}
}

func TestGitignoreTree(t *testing.T) {
	root := t.TempDir()
	// root .gitignore
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\nbuild/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// nested .gitignore in src/
	srcDir := filepath.Join(root, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".gitignore"), []byte("*.generated.go\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := NewGitignoreTree(root)
	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"app.log", false, true},
		{"deep/trace.log", false, true},
		{"build", true, true},
		{"build/out.js", false, true},
		{"main.go", false, false},
		{"src/handler.go", false, false},
		{"src/types.generated.go", false, true},
		{"src/sub/deep.generated.go", false, true},
		{"types.generated.go", false, false}, // only src/.gitignore has this rule
	}
	for _, c := range cases {
		if got := g.Ignored(c.path, c.isDir); got != c.want {
			t.Errorf("GitignoreTree.Ignored(%q, dir=%v) = %v, want %v", c.path, c.isDir, got, c.want)
		}
	}
}

func TestGitignoreTreeNoFile(t *testing.T) {
	root := t.TempDir()
	g := NewGitignoreTree(root)
	if g.Ignored("anything.go", false) {
		t.Error("should not ignore anything without .gitignore")
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
