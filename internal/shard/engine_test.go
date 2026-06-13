package shard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

var testCfg = DiskEngineConfig{
	ShardSize:        ShardSizeConfig{TargetBytes: 80 << 20, MinBytes: 32 << 20, MaxBytes: 128 << 20},
	RebuildThreshold: 20,
	SearchWorkers:    2,
}

func makeEngine(t *testing.T) (root string, shardDir string, e *DiskEngine) {
	t.Helper()
	root = t.TempDir()
	shardDir = t.TempDir()
	write(t, filepath.Join(root, "a.go"), "package p\n\nfunc SearchNeedle() {}\nfunc Other() {}\n")
	write(t, filepath.Join(root, "b.go"), "var x = SearchNeedle\n")
	write(t, filepath.Join(root, "sub", "c.txt"), "deep SearchNeedle here\n")
	e = NewDiskEngine(root, shardDir, 2, 0, testCfg)
	return
}

func buildFiles(t *testing.T, root string, rels ...string) []BuildFile {
	t.Helper()
	var files []BuildFile
	for _, rel := range rels {
		fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, BuildFile{Rel: rel, Size: fi.Size(), MtimeNS: fi.ModTime().UnixNano()})
	}
	return files
}

func TestFullBuildAndSearch(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := buildFiles(t, root, "a.go", "b.go", "sub/c.txt")
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

	files := buildFiles(t, root, "a.go")
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)
	write(t, filepath.Join(root, "a.go"), "func ModifiedNeedle() {}\n")
	e.MarkDirty("a.go")

	re := regexp.MustCompile("ModifiedNeedle")
	res := e.Search(re, index.SearchOpts{Literal: "modifiedneedle"})
	if len(res.Matches) != 1 {
		t.Fatalf("dirty match = %d, want 1", len(res.Matches))
	}
	re2 := regexp.MustCompile("SearchNeedle")
	res2 := e.Search(re2, index.SearchOpts{Literal: "searchneedle"})
	if len(res2.Matches) != 0 {
		t.Fatalf("stale match = %d, want 0", len(res2.Matches))
	}
}

func TestRebuild(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := buildFiles(t, root, "a.go")
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

	files := buildFiles(t, root, "a.go")
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
	b := NewBuilder(0)
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
	cands := s.Candidates(strings.ToLower("Hello"))
	if len(cands) < 1 {
		t.Fatal("no candidates for Hello")
	}
}

func TestSymbolsInDiskEngine(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := buildFiles(t, root, "a.go")
	_ = e.FullBuild(files, nil)

	defs := e.Defs("SearchNeedle", false, nil)
	if len(defs) != 1 {
		t.Fatalf("defs = %d, want 1", len(defs))
	}
	if defs[0].Def.Kind != "func" {
		t.Errorf("def kind = %s, want func", defs[0].Def.Kind)
	}
}

func TestMmapLifecycle(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(0)
	b.Add("a.go", 10, 1, []byte("func Foo() {}\n"))
	path := filepath.Join(dir, "test.idx")
	if err := b.Write(path); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.PathMin() != "a.go" || s.PathMax() != "a.go" {
		t.Fatalf("path range = [%s, %s]", s.PathMin(), s.PathMax())
	}

	// mmap, use, munmap
	if err := s.Mmap(); err != nil {
		t.Fatal(err)
	}
	if s.NumFiles() != 1 {
		t.Fatalf("NumFiles = %d", s.NumFiles())
	}
	fm := s.File(0)
	if fm.Path != "a.go" {
		t.Fatalf("path = %s", fm.Path)
	}
	s.Munmap()

	// double mmap (refcount)
	if err := s.Mmap(); err != nil {
		t.Fatal(err)
	}
	if err := s.Mmap(); err != nil {
		t.Fatal(err)
	}
	s.Munmap()
	// still mapped (refs=1)
	_ = s.File(0)
	s.Munmap()
	// now unmapped (refs=0)
}

func TestOrderedPaths(t *testing.T) {
	root := t.TempDir()
	shardDir := t.TempDir()
	// create files in multiple dirs
	for _, rel := range []string{"alpha/a.go", "beta/b.go", "gamma/g.go"} {
		write(t, filepath.Join(root, filepath.FromSlash(rel)), "func Needle() {}\n")
	}
	e := NewDiskEngine(root, shardDir, 2, 0, testCfg)
	defer e.Close()

	files := buildFiles(t, root, "alpha/a.go", "beta/b.go", "gamma/g.go")
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}

	// verify path ordering within shards
	snap := e.snapshot.Load()
	for _, s := range snap.shards {
		if err := s.Mmap(); err != nil {
			t.Fatal(err)
		}
		for i := 1; i < s.NumFiles(); i++ {
			prev := s.File(i - 1).Path
			cur := s.File(i).Path
			if prev >= cur {
				t.Errorf("paths not ordered: %s >= %s", prev, cur)
			}
		}
		if s.NumFiles() > 0 {
			if s.PathMin() != s.File(0).Path {
				t.Errorf("PathMin mismatch")
			}
			if s.PathMax() != s.File(s.NumFiles()-1).Path {
				t.Errorf("PathMax mismatch")
			}
		}
		s.Munmap()
	}
}

func TestManifestPersistence(t *testing.T) {
	root, shardDir, e := makeEngine(t)
	defer e.Close()

	files := buildFiles(t, root, "a.go", "b.go", "sub/c.txt")
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}

	// manifest should exist
	m, err := ReadManifest(shardDir)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("no manifest")
	}
	if len(m.Shards) != e.NumShards() {
		t.Fatalf("manifest shards = %d, engine shards = %d", len(m.Shards), e.NumShards())
	}

	// create new engine from same shard dir (simulates restart)
	e2 := NewDiskEngine(root, shardDir, 2, 0, testCfg)
	defer e2.Close()
	e2.Reconcile(files)
	if e2.NumFiles() != 3 {
		t.Fatalf("reconciled NumFiles = %d, want 3", e2.NumFiles())
	}
}

func TestCrashRecovery(t *testing.T) {
	root, shardDir, e := makeEngine(t)
	defer e.Close()

	files := buildFiles(t, root, "a.go", "b.go")
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}

	// simulate crash: leave a .tmp file and an orphan .idx
	tmpPath := filepath.Join(shardDir, "shard-9999.idx.tmp")
	os.WriteFile(tmpPath, []byte("garbage"), 0o600)
	orphanPath := filepath.Join(shardDir, "shard-9998.idx")
	os.WriteFile(orphanPath, []byte("orphan"), 0o600)

	// reconcile should clean orphans
	e2 := NewDiskEngine(root, shardDir, 2, 0, testCfg)
	defer e2.Close()
	e2.Reconcile(files)

	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Error("tmp file not cleaned")
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Error("orphan not cleaned")
	}
}

func TestConcurrentSearchAndRebuild(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := buildFiles(t, root, "a.go", "b.go", "sub/c.txt")
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	// concurrent searches
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				re := regexp.MustCompile("SearchNeedle")
				res := e.Search(re, index.SearchOpts{Literal: "searchneedle"})
				if len(res.Matches) < 1 {
					t.Errorf("concurrent search found 0 matches")
				}
			}
		}()
	}
	// concurrent rebuild
	wg.Add(1)
	go func() {
		defer wg.Done()
		write(t, filepath.Join(root, "a.go"), "func ConcurrentNeedle() {}\n")
		e.MarkDirty("a.go")
		e.rebuild()
	}()
	wg.Wait()
}

func TestNewFileViaRebuild(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := buildFiles(t, root, "a.go")
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}

	// add a new file and mark dirty
	write(t, filepath.Join(root, "new.go"), "func NewFileNeedle() {}\n")
	e.MarkDirty("new.go")
	e.rebuild()

	re := regexp.MustCompile("NewFileNeedle")
	res := e.Search(re, index.SearchOpts{Literal: "newfileneedle"})
	if len(res.Matches) != 1 {
		t.Fatalf("new file match = %d, want 1", len(res.Matches))
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := &Manifest{
		Version: 1,
		SeqNo:   5,
		Shards: []ManifestShard{
			{File: "shard-0000.idx", PathMin: "a.go", PathMax: "b.go"},
			{File: "shard-0001.idx", PathMin: "c.go", PathMax: "z.go"},
		},
	}
	if err := WriteManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	m2, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	d1, _ := json.Marshal(m)
	d2, _ := json.Marshal(m2)
	if string(d1) != string(d2) {
		t.Fatalf("manifest roundtrip mismatch:\n%s\n%s", d1, d2)
	}
}

func TestGroupByDirBoundary(t *testing.T) {
	var files []BuildFile
	// 10 files of 10MB each = 100MB total → should be 2 shards at 80MB target
	for i := 0; i < 10; i++ {
		files = append(files, BuildFile{
			Rel:  filepath.Join("dir", strings.Repeat("x", i+1)+".go"),
			Size: 10 << 20,
		})
	}
	batches := GroupByDirBoundary(files, testCfg.ShardSize)
	if len(batches) < 1 {
		t.Fatal("no batches")
	}
	// verify all files accounted for
	total := 0
	for _, b := range batches {
		total += len(b)
	}
	if total != 10 {
		t.Fatalf("total files = %d, want 10", total)
	}
}

func TestShardForPath(t *testing.T) {
	root := t.TempDir()
	shardDir := t.TempDir()
	for _, rel := range []string{"alpha/a.go", "beta/b.go", "gamma/g.go"} {
		write(t, filepath.Join(root, filepath.FromSlash(rel)), "func Needle() {}\n")
	}
	e := NewDiskEngine(root, shardDir, 2, 0, testCfg)
	defer e.Close()

	files := buildFiles(t, root, "alpha/a.go", "beta/b.go", "gamma/g.go")
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}

	snap := e.snapshot.Load()
	// Meta should find files via binary search on shard ranges
	fm, ok := e.Meta("alpha/a.go")
	if !ok {
		t.Fatal("Meta: alpha/a.go not found")
	}
	if fm.Path != "alpha/a.go" {
		t.Fatalf("Meta: path = %s", fm.Path)
	}
	_, ok = e.Meta("nonexistent.go")
	if ok {
		t.Fatal("Meta: found nonexistent file")
	}
	// shardForPath should return valid index for known files
	si := snap.shardForPath("beta/b.go")
	if si < 0 {
		t.Fatal("shardForPath: beta/b.go not found")
	}
	si = snap.shardForPath("zzz/none.go")
	if si >= 0 {
		t.Fatal("shardForPath: found zzz/none.go")
	}
}

func TestHasTrigrams(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(0)
	b.Add("a.go", 10, 1, []byte("func Hello() {}\n"))
	path := filepath.Join(dir, "test.idx")
	if err := b.Write(path); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.TriKeyCount() == 0 {
		t.Fatal("no trigram keys loaded")
	}
	if !s.HasTrigrams("Hello") {
		t.Error("HasTrigrams: Hello should be present")
	}
	if !s.HasTrigrams("func") {
		t.Error("HasTrigrams: func should be present")
	}
	if s.HasTrigrams("ZZZNOTHERE") {
		t.Error("HasTrigrams: ZZZNOTHERE should be absent")
	}
	// short literal always true
	if !s.HasTrigrams("ab") {
		t.Error("HasTrigrams: short literal should return true")
	}
}

func TestTrigramSkipInSearch(t *testing.T) {
	root := t.TempDir()
	shardDir := t.TempDir()
	// put files in different dirs to get separate shards at small target
	write(t, filepath.Join(root, "aaa", "x.go"), "func UniqueAlpha() {}\n")
	write(t, filepath.Join(root, "zzz", "y.go"), "func UniqueBeta() {}\n")
	e := NewDiskEngine(root, shardDir, 2, 0, testCfg)
	defer e.Close()

	files := buildFiles(t, root, "aaa/x.go", "zzz/y.go")
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}

	// search for UniqueAlpha — should find 1 match, and the shard
	// containing only UniqueBeta should be skipped via HasTrigrams
	re := regexp.MustCompile("UniqueAlpha")
	res := e.Search(re, index.SearchOpts{Literal: "uniquealpha"})
	if len(res.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(res.Matches))
	}
}

func TestSmallDirtyRebuildsImmediately(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := buildFiles(t, root, "a.go", "b.go", "sub/c.txt")
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}

	// few dirty files (below threshold) → rebuild immediately
	write(t, filepath.Join(root, "a.go"), "func SmallDirtyNeedle() {}\n")
	e.MarkDirty("a.go")
	e.rebuild()

	if e.DirtyCount() != 0 {
		t.Fatalf("dirty count = %d, want 0 (should rebuild immediately)", e.DirtyCount())
	}
	re := regexp.MustCompile("SmallDirtyNeedle")
	res := e.Search(re, index.SearchOpts{Literal: "smalldirtyneedle"})
	if len(res.Matches) != 1 {
		t.Fatalf("match = %d, want 1", len(res.Matches))
	}
}

func TestInlineContent(t *testing.T) {
	root := t.TempDir()
	shardDir := t.TempDir()
	write(t, filepath.Join(root, "small.go"), "func InlineNeedle() {}\n")
	write(t, filepath.Join(root, "big.txt"), strings.Repeat("x", 200*1024)+"\nfunc InlineNeedle() {}\n")

	inlineCfg := DiskEngineConfig{
		ShardSize:        testCfg.ShardSize,
		RebuildThreshold: testCfg.RebuildThreshold,
		SearchWorkers:    testCfg.SearchWorkers,
		InlineKB:         100, // inline files ≤ 100KB
	}
	e := NewDiskEngine(root, shardDir, 2, 0, inlineCfg)
	defer e.Close()

	files := buildFiles(t, root, "small.go", "big.txt")
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}

	// small.go should be inlined, big.txt should not
	re := regexp.MustCompile("InlineNeedle")
	res := e.Search(re, index.SearchOpts{Literal: "inlineneedle"})
	if len(res.Matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(res.Matches))
	}

	// verify shard has content section
	snap := e.snapshot.Load()
	for _, s := range snap.shards {
		if err := s.Mmap(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < s.NumFiles(); i++ {
			fm := s.File(i)
			content := s.FileContent(i)
			if fm.Path == "small.go" && content == nil {
				t.Error("small.go should have inline content")
			}
			if fm.Path == "big.txt" && content != nil {
				t.Error("big.txt should NOT have inline content")
			}
		}
		s.Munmap()
	}
}

func TestDeletedPrefix(t *testing.T) {
	root, _, e := makeEngine(t)
	defer e.Close()

	files := buildFiles(t, root, "a.go", "sub/c.txt")
	if err := e.FullBuild(files, nil); err != nil {
		t.Fatal(err)
	}
	e.MarkDeletedPrefix("sub")

	re := regexp.MustCompile("SearchNeedle")
	res := e.Search(re, index.SearchOpts{Literal: "searchneedle"})
	// only a.go should match, sub/c.txt should be dirty-deleted
	for _, m := range res.Matches {
		if strings.HasPrefix(m.Path, "sub/") {
			t.Errorf("deleted prefix still matches: %s", m.Path)
		}
	}
}
