package ignore

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// GitignoreTree checks paths against .gitignore files found throughout
// the directory tree. Each .gitignore applies to its own directory and
// descendants. Thread-safe: directories are loaded lazily and cached.
type GitignoreTree struct {
	root string
	mu   sync.RWMutex
	dirs map[string][]rule // dir (slash-separated, relative) → rules
}

// NewGitignoreTree creates a tree rooted at root. Call Ignored() to
// check individual paths; .gitignore files are loaded on demand.
func NewGitignoreTree(root string) *GitignoreTree {
	return &GitignoreTree{root: root, dirs: make(map[string][]rule)}
}

// Ignored reports whether the slash-separated relative path is excluded
// by any .gitignore in its ancestor chain.
func (g *GitignoreTree) Ignored(rel string, isDir bool) bool {
	parts := strings.Split(rel, "/")
	// check each ancestor's .gitignore
	for i := 0; i <= len(parts)-1; i++ {
		var dir string
		if i == 0 {
			dir = "."
		} else {
			dir = strings.Join(parts[:i], "/")
		}
		rules := g.loadDir(dir)
		// the rel path relative to this .gitignore's directory
		var sub string
		if i == 0 {
			sub = rel
		} else {
			sub = strings.Join(parts[i:], "/")
		}
		for _, r := range rules {
			if r.prefix.MatchString(sub) {
				return true
			}
			if r.exact.MatchString(sub) && (isDir || !r.dirOnly) {
				return true
			}
		}
	}
	return false
}

func (g *GitignoreTree) loadDir(dir string) []rule {
	g.mu.RLock()
	rules, ok := g.dirs[dir]
	g.mu.RUnlock()
	if ok {
		return rules
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if rules, ok = g.dirs[dir]; ok {
		return rules
	}
	var absDir string
	if dir == "." {
		absDir = g.root
	} else {
		absDir = filepath.Join(g.root, filepath.FromSlash(dir))
	}
	rules = loadGitignoreFile(filepath.Join(absDir, ".gitignore"))
	g.dirs[dir] = rules
	return rules
}

func loadGitignoreFile(path string) []rule {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rules []rule
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if r, ok := compile(line); ok {
			rules = append(rules, r)
		}
	}
	return rules
}
