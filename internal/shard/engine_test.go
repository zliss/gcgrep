package shard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zliss/gcgrep/internal/index"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func makeEngine(t *testing.T) (root string, shardDir string, e *DiskEngine) {
	t.Helper()
	root = t.TempDir()
	shardDir = t.TempDir()
	write(t, filepath.Join(root, "a.go"), "package p\n\nfunc SearchNeedle() {}\nfunc Other() {}\n")
	write(t, filepath.Join(root, "b.go"), "var x = SearchNeedle\n")
	write(t, filepath.Join(root, "sub", "c.txt"), "deep SearchNeedle here\n")
	e = NewDiskEngine(root, shardDir, 2, 0)
	return
}

func TestFullBuildAndSearch(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := []BuildFile{
		{Rel: "a.go", Size: 40, MtimeNS: 1},
		{Rel: "b.go", Size: 22, MtimeNS: 1},
		{Rel: "sub/c.txt", Size: 23, MtimeNS: 1},
	}
	for i := range files {
		fi, _ := os.Stat(filepath.Join(root, filepath.FromSlash(files[i].Rel)))
		files[i].Size = fi.Size()
		files[i].MtimeNS = fi.ModTime().UnixNano()
	}
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}
	if e.NumFiles() != 3 {
		t.Fatalf("NumFiles = %d, want 3", e.NumFiles())
	}
	if e.NumShards() < 1 {
		t.Fatal("no shards built")
	}

	re := regexp.MustCompile("SearchNeedle")
	res := e.Search(re, index.SearchOpts{Literal: "searchneedle"})
	if len(res.Matches) != 3 {
		t.Fatalf("matches = %d, want 3; matches: %+v", len(res.Matches), res.Matches)
	}
}

func TestDirtyListSearch(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := []BuildFile{
		{Rel: "a.go", Size: 40, MtimeNS: 1},
	}
	fi, _ := os.Stat(filepath.Join(root, "a.go"))
	files[0].Size = fi.Size()
	files[0].MtimeNS = fi.ModTime().UnixNano()
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}

	// modify file after build
	time.Sleep(10 * time.Millisecond)
	write(t, filepath.Join(root, "a.go"), "func ModifiedNeedle() {}\n")
	e.MarkDirty("a.go")

	// should find the new content via dirty scan
	re := regexp.MustCompile("ModifiedNeedle")
	res := e.Search(re, index.SearchOpts{Literal: "modifiedneedle"})
	if len(res.Matches) != 1 {
		t.Fatalf("dirty match = %d, want 1", len(res.Matches))
	}
	// old content should NOT match (dirty file excluded from shard results)
	re2 := regexp.MustCompile("SearchNeedle")
	res2 := e.Search(re2, index.SearchOpts{Literal: "searchneedle"})
	if len(res2.Matches) != 0 {
		t.Fatalf("stale match = %d, want 0", len(res2.Matches))
	}
}

func TestRebuild(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := []BuildFile{{Rel: "a.go", Size: 40, MtimeNS: 1}}
	fi, _ := os.Stat(filepath.Join(root, "a.go"))
	files[0].Size = fi.Size()
	files[0].MtimeNS = fi.ModTime().UnixNano()
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "a.go"), "func RebuildNeedle() {}\n")
	e.MarkDirty("a.go")
	if e.DirtyCount() != 1 {
		t.Fatal("dirty count wrong before rebuild")
	}

	e.rebuild()

	if e.DirtyCount() != 0 {
		t.Fatal("dirty count not cleared after rebuild")
	}
	re := regexp.MustCompile("RebuildNeedle")
	res := e.Search(re, index.SearchOpts{Literal: "rebuildneedle"})
	if len(res.Matches) != 1 {
		t.Fatalf("post-rebuild match = %d, want 1", len(res.Matches))
	}
}

func TestDeletedFile(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := []BuildFile{{Rel: "a.go", Size: 40, MtimeNS: 1}}
	fi, _ := os.Stat(filepath.Join(root, "a.go"))
	files[0].Size = fi.Size()
	files[0].MtimeNS = fi.ModTime().UnixNano()
	_ = e.FullBuild(files, nil)
	os.Remove(filepath.Join(root, "a.go"))
	e.MarkDeleted("a.go")

	re := regexp.MustCompile("SearchNeedle")
	res := e.Search(re, index.SearchOpts{Literal: "searchneedle"})
	if len(res.Matches) != 0 {
		t.Fatalf("deleted file still matches: %+v", res.Matches)
	}
}

func TestShardFormat(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder()
	b.Add("hello.go", 100, 999, []byte("func Hello() {}\nvar x = 42\n"))
	b.Add("world.go", 200, 888, []byte("func World() {}\n"))
	path := filepath.Join(dir, "test.idx")
	if err := b.Write(path); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.NumFiles() != 2 {
		t.Fatalf("NumFiles = %d, want 2", s.NumFiles())
	}
	f0 := s.File(0)
	if f0.Path != "hello.go" || f0.Size != 100 {
		t.Fatalf("file 0 = %+v", f0)
	}
	// "Hello" trigram should find file 0
	cands := s.Candidates(strings.ToLower("Hello"))
	if len(cands) < 1 {
		t.Fatal("no candidates for Hello")
	}
}

func TestSymbolsInDiskEngine(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := []BuildFile{{Rel: "a.go", Size: 40, MtimeNS: 1}}
	fi, _ := os.Stat(filepath.Join(root, "a.go"))
	files[0].Size = fi.Size()
	files[0].MtimeNS = fi.ModTime().UnixNano()
	_ = e.FullBuild(files, nil)

	defs := e.Defs("SearchNeedle", false, nil)
	if len(defs) != 1 {
		t.Fatalf("defs = %d, want 1", len(defs))
	}
	if defs[0].Def.Kind != "func" {
		t.Errorf("def kind = %s, want func", defs[0].Def.Kind)
	}
}
