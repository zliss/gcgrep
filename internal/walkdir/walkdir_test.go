package walkdir

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

func collect(t *testing.T, root string, follow bool) []string {
	t.Helper()
	var out []string
	err := Walk(root, follow, func(path string, isDir bool, fi os.FileInfo) error {
		if !isDir {
			rel, _ := filepath.Rel(root, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need privileges on windows:", err)
		}
		t.Fatal(err)
	}
}

func TestWalkFollow(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "ext.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "sub/b.txt"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(f)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	symlink(t, outside, filepath.Join(root, "linkdir"))
	symlink(t, filepath.Join(outside, "ext.txt"), filepath.Join(root, "linkfile.txt"))
	// cycle: symlink back to root must not loop
	symlink(t, root, filepath.Join(root, "sub", "cycle"))

	noFollow := collect(t, root, false)
	want := []string{"a.txt", "sub/b.txt"}
	if len(noFollow) != len(want) || noFollow[0] != want[0] || noFollow[1] != want[1] {
		t.Errorf("follow=false files = %v, want %v", noFollow, want)
	}

	followed := collect(t, root, true)
	wantF := []string{"a.txt", "linkdir/ext.txt", "linkfile.txt", "sub/b.txt"}
	if len(followed) != len(wantF) {
		t.Fatalf("follow=true files = %v, want %v", followed, wantF)
	}
	for i := range wantF {
		if followed[i] != wantF[i] {
			t.Errorf("follow=true files = %v, want %v", followed, wantF)
			break
		}
	}
}
