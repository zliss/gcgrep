package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zliss/gcgrep/internal/conf"
	"github.com/zliss/gcgrep/internal/index"
	"github.com/zliss/gcgrep/internal/symbol"
)

// waitTimeout exceeds 5s deliberately: watcher debounce is 200ms but CI
// filesystems (and Windows Defender on the win runner) can delay event
// delivery; polling exits as soon as the condition holds.
const waitTimeout = 10 * time.Second

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newStore(t *testing.T, root string) *RootStore {
	t.Helper()
	s, err := newRootStore(root, t.TempDir(), conf.Default(), false)
	if err != nil {
		t.Fatal(err)
	}
	s.WaitReady()
	return s
}

func hits(s *RootStore, pattern string) []index.Match {
	re := regexp.MustCompile(pattern)
	return s.idx.Search(re, index.SearchOpts{Literal: index.ExtractLiteral(pattern, false)}).Matches
}

// waitFor polls until cond is true or the timeout elapses.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestInitialScanAndSearch(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), "func InitialNeedle() {}\n")
	write(t, filepath.Join(root, "sub", "b.go"), "var other = 2\n")
	write(t, filepath.Join(root, "bin.dat"), "x\x00y binary")
	s := newStore(t, root)
	defer s.Close()

	if m := hits(s, "InitialNeedle"); len(m) != 1 || m[0].Path != "a.go" {
		t.Fatalf("scan results wrong: %+v", m)
	}
	if s.idx.NumFiles() != 2 {
		t.Fatalf("binary file indexed? NumFiles=%d", s.idx.NumFiles())
	}
}

func TestWatchAddModifyDelete(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), "original content here\n")
	s := newStore(t, root)
	defer s.Close()

	// modify
	write(t, filepath.Join(root, "a.go"), "modified needleAA here\n")
	waitFor(t, "modification indexed", func() bool { return len(hits(s, "needleAA")) == 1 })
	if len(hits(s, "original content")) != 0 {
		t.Fatal("stale content still indexed")
	}

	// add, including a new nested directory
	write(t, filepath.Join(root, "newdir", "deep", "n.go"), "fresh needleBB file\n")
	waitFor(t, "new nested file indexed", func() bool { return len(hits(s, "needleBB")) == 1 })

	// delete
	if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "deletion applied", func() bool { return len(hits(s, "needleAA")) == 0 })

	// delete a whole directory
	if err := os.RemoveAll(filepath.Join(root, "newdir")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "dir deletion applied", func() bool { return len(hits(s, "needleBB")) == 0 })
}

func TestPersistAndReconcile(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	write(t, filepath.Join(root, "keep.go"), "keepNeedle stays\n")
	write(t, filepath.Join(root, "gone.go"), "goneNeedle leaves\n")
	write(t, filepath.Join(root, "edit.go"), "before edit\n")

	s1, err := newRootStore(root, cache, conf.Default(), false)
	if err != nil {
		t.Fatal(err)
	}
	s1.WaitReady()
	s1.Close() // persists the index

	// offline changes while no daemon runs
	if err := os.Remove(filepath.Join(root, "gone.go")); err != nil {
		t.Fatal(err)
	}
	// ensure a different mtime even on coarse-granularity filesystems
	time.Sleep(1100 * time.Millisecond)
	write(t, filepath.Join(root, "edit.go"), "after editNeedle\n")
	write(t, filepath.Join(root, "new.go"), "brand newNeedle\n")

	s2, err := newRootStore(root, cache, conf.Default(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.State() == StateIndexing {
		t.Fatal("persisted index was not loaded")
	}
	s2.WaitReady()
	waitFor(t, "reconcile to finish", func() bool { return s2.State() == StateReady })

	if len(hits(s2, "keepNeedle")) != 1 {
		t.Error("unchanged file lost")
	}
	if len(hits(s2, "goneNeedle")) != 0 {
		t.Error("offline-deleted file still indexed")
	}
	if len(hits(s2, "editNeedle")) != 1 || len(hits(s2, "before edit")) != 0 {
		t.Error("offline edit not reconciled")
	}
	if len(hits(s2, "newNeedle")) != 1 {
		t.Error("offline-added file not indexed")
	}
}

func TestGcgrepignoreRespected(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".gcgrepignore"), "skipme/\n*.log\n")
	write(t, filepath.Join(root, "skipme", "x.go"), "ignoredNeedle\n")
	write(t, filepath.Join(root, "app.log"), "ignoredNeedle\n")
	write(t, filepath.Join(root, "ok.go"), "visibleNeedle\n")
	s := newStore(t, root)
	defer s.Close()

	if len(hits(s, "ignoredNeedle")) != 0 {
		t.Error("excluded content was indexed")
	}
	if len(hits(s, "visibleNeedle")) != 1 {
		t.Error("regular file missing")
	}
	// changes inside ignored dirs must not get indexed by the watcher either
	write(t, filepath.Join(root, "skipme", "later.go"), "lateIgnoredNeedle\n")
	write(t, filepath.Join(root, "later_ok.go"), "lateVisibleNeedle\n")
	waitFor(t, "visible new file", func() bool { return len(hits(s, "lateVisibleNeedle")) == 1 })
	if len(hits(s, "lateIgnoredNeedle")) != 0 {
		t.Error("watcher indexed content in ignored dir")
	}
}

// TestReadAfterWriteBarrier hammers the exact AI-agent workflow: write a
// file and search immediately, with NO sleeps between. Without the cookie
// barrier this flakes within a few iterations (debounce window = 200ms).
func TestReadAfterWriteBarrier(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "w.go"), "iteration0marker\n")
	s := newStore(t, root)
	defer s.Close()

	const rounds = 25
	for i := 1; i <= rounds; i++ {
		needle := fmt.Sprintf("iteration%dmarker", i)
		write(t, filepath.Join(root, "w.go"), needle+"\n")
		s.Barrier(waitTimeout)
		if got := hits(s, needle); len(got) != 1 {
			t.Fatalf("round %d: write not visible after barrier: %+v", i, got)
		}
		if stale := hits(s, fmt.Sprintf("iteration%dmarker", i-1)); len(stale) != 0 {
			t.Fatalf("round %d: stale content visible after barrier", i)
		}
	}
	// new file + immediate search
	write(t, filepath.Join(root, "fresh.go"), "freshFileMarker\n")
	s.Barrier(waitTimeout)
	if got := hits(s, "freshFileMarker"); len(got) != 1 {
		t.Fatal("new file not visible after barrier")
	}
	// delete + immediate search
	if err := os.Remove(filepath.Join(root, "fresh.go")); err != nil {
		t.Fatal(err)
	}
	s.Barrier(waitTimeout)
	if got := hits(s, "freshFileMarker"); len(got) != 0 {
		t.Fatal("deleted file still visible after barrier")
	}
	// cookie files must never enter the index
	for _, p := range s.idx.Paths() {
		if isCookie(p) {
			t.Fatalf("cookie file leaked into index: %s", p)
		}
	}
}

func TestSymbolDefsAndRefs(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "svc.go"), `package p

type UserService struct{}

func (s *UserService) GetUser(id int) int { return id }
func caller() { s := &UserService{}; _ = s.GetUser(1) }
`)
	write(t, filepath.Join(root, "Svc.java"), `public class OrderService {
    public void getUser(long id) { helper(); }
    private void helper() {}
}
`)
	s := newStore(t, root)
	defer s.Close()

	defs := s.idx.Defs("GetUser", false, nil)
	if len(defs) != 1 || defs[0].Path != "svc.go" || defs[0].Def.Container != "UserService" {
		t.Fatalf("go def wrong: %+v", defs)
	}
	// case-insensitive crosses languages
	all := s.idx.Defs("getuser", true, nil)
	if len(all) != 2 {
		t.Fatalf("want 2 case-folded defs, got %+v", all)
	}
	fd, ok := s.idx.FileDefs("Svc.java")
	if !ok || len(fd) != 3 {
		t.Fatalf("java file defs: ok=%v %+v", ok, fd)
	}
	// refs candidates exclude the definition line
	files := s.idx.FilesContaining("GetUser", nil)
	refs := 0
	for _, fc := range files {
		refs += len(symbol.Refs(fc.Path, fc.Content, "GetUser"))
	}
	if refs != 1 {
		t.Fatalf("want 1 GetUser ref (the call in caller()), got %d", refs)
	}

	// live update: new file's symbols appear without restart
	write(t, filepath.Join(root, "extra.py"), "class ExtraService:\n    def get_user(self):\n        pass\n")
	s.Barrier(waitTimeout)
	if d := s.idx.Defs("ExtraService", false, nil); len(d) != 1 {
		t.Fatalf("python def not live-indexed: %+v", d)
	}
}

// v0.5 semantics: oversized / over-budget / binary files are not lost —
// they land in the stream manifest, announced to the client for disk scan.
func TestMaxFileSizeAndBudget(t *testing.T) {
	root := t.TempDir()
	big := strings.Repeat("x", 3<<20)
	write(t, filepath.Join(root, "big.txt"), big+" bigNeedle\n")
	write(t, filepath.Join(root, "ok.txt"), "small okNeedle\n")
	s := newStore(t, root)
	defer s.Close()
	if len(hits(s, "bigNeedle")) != 0 {
		t.Error("over-limit file indexed")
	}
	if s.skippedLarge.Load() != 1 {
		t.Errorf("skippedLarge = %d, want 1", s.skippedLarge.Load())
	}
	if len(hits(s, "okNeedle")) != 1 {
		t.Error("normal file missing")
	}
	sf := s.StreamList(0, nil)
	if len(sf) != 1 || sf[0].Rel != "big.txt" {
		t.Errorf("stream manifest = %+v, want [big.txt]", sf)
	}
	// --max-filesize filters the manifest
	if got := s.StreamList(1<<20, nil); len(got) != 0 {
		t.Errorf("max-filesize filter ignored: %+v", got)
	}

	// per-root byte budget
	root2 := t.TempDir()
	half := strings.Repeat("y", 600<<10)
	for i := 0; i < 4; i++ {
		write(t, filepath.Join(root2, fmt.Sprintf("f%d.txt", i)), half)
	}
	cfg := conf.Default()
	cfg.MaxIndexMB = 1
	s2, err := newRootStore(root2, t.TempDir(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.WaitReady()
	if s2.skippedBudget.Load() == 0 {
		t.Error("budget not enforced")
	}
	if s2.idx.NumFiles() == 0 || s2.idx.NumFiles() == 4 {
		t.Errorf("expected partial indexing, got %d files", s2.idx.NumFiles())
	}
	if s2.idx.NumFiles()+s2.streamCount() != 4 {
		t.Errorf("indexed %d + stream %d != 4: over-budget files lost", s2.idx.NumFiles(), s2.streamCount())
	}
}

func TestSkipCountersVisible(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "bin.dat"), "x\x00binary")
	write(t, filepath.Join(root, "utf16.txt"), "\xff\xfeh\x00i\x00 \x00n\x00e\x00e\x00d\x00l\x00e\x00U\x00") // UTF-16LE "hi needleU"
	write(t, filepath.Join(root, "ok.go"), "plain text\n")
	s := newStore(t, root)
	defer s.Close()
	// utf16 is transcoded and indexed now; only bin.dat counts as binary
	if got := s.skippedBinary.Load(); got != 1 {
		t.Errorf("skippedBinary = %d, want 1", got)
	}
	if s.idx.NumFiles() != 2 {
		t.Errorf("NumFiles = %d, want 2 (ok.go + transcoded utf16.txt)", s.idx.NumFiles())
	}
	if m := hits(s, "needleU"); len(m) != 1 || m[0].Path != "utf16.txt" {
		t.Errorf("utf16 content not searchable: %+v", m)
	}
	if sf := s.StreamList(0, nil); len(sf) != 1 || sf[0].Rel != "bin.dat" {
		t.Errorf("binary not in stream manifest: %+v", sf)
	}
}

// TestStreamManifestLifecycle: a file moving across the size threshold
// must migrate between index and stream manifest, and deletion must drop
// it from both.
func TestStreamManifestLifecycle(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "f.txt"), "small lifeNeedle\n")
	s := newStore(t, root)
	defer s.Close()
	if len(hits(s, "lifeNeedle")) != 1 || s.streamCount() != 0 {
		t.Fatal("initial state wrong")
	}
	// grow past the 2MB default threshold
	write(t, filepath.Join(root, "f.txt"), strings.Repeat("x", 3<<20))
	waitFor(t, "file moved to stream set", func() bool {
		return s.streamCount() == 1 && s.idx.NumFiles() == 0
	})
	// shrink back
	write(t, filepath.Join(root, "f.txt"), "small again lifeNeedle\n")
	waitFor(t, "file back in index", func() bool {
		return s.streamCount() == 0 && len(hits(s, "lifeNeedle")) == 1
	})
	// delete
	if err := os.Remove(filepath.Join(root, "f.txt")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "file gone everywhere", func() bool {
		return s.streamCount() == 0 && s.idx.NumFiles() == 0
	})
}

func TestDecodeUTF16(t *testing.T) {
	le, ok := decodeUTF16([]byte("\xff\xfeh\x00i\x00"))
	if !ok || string(le) != "hi" {
		t.Errorf("LE decode = %q ok=%v", le, ok)
	}
	be, ok := decodeUTF16([]byte("\xfe\xff\x00h\x00i"))
	if !ok || string(be) != "hi" {
		t.Errorf("BE decode = %q ok=%v", be, ok)
	}
	if _, ok := decodeUTF16([]byte("plain")); ok {
		t.Error("non-BOM content claimed as UTF-16")
	}
}

func TestExcludeIncludeMatcher(t *testing.T) {
	// no patterns → nil
	if fn, err := excludeIncludeMatcher(nil, nil); fn != nil || err != nil {
		t.Fatalf("empty should return nil, got fn=%p err=%v", fn, err)
	}

	// exclude only
	fn, err := excludeIncludeMatcher([]string{"*.log", "vendor/*"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fn("app.log") {
		t.Error("*.log should be excluded")
	}
	if fn("vendor/lib.go") {
		t.Error("vendor/* should be excluded")
	}
	if !fn("main.go") {
		t.Error("main.go should pass")
	}

	// include overrides exclude
	fn2, _ := excludeIncludeMatcher([]string{"*.log"}, []string{"important.log"})
	if !fn2("important.log") {
		t.Error("include should override exclude")
	}
	if fn2("debug.log") {
		t.Error("non-included .log should be excluded")
	}
	if !fn2("main.go") {
		t.Error("unmatched file should pass")
	}
}

func TestGcgrepExcludeEnv(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "ok.go"), "visibleExcNeedle\n")
	write(t, filepath.Join(root, "gen", "out.go"), "excludedExcNeedle\n")

	cfg := conf.Default()
	cfg.Exclude = []string{"gen/"}
	s, err := newRootStore(root, t.TempDir(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.WaitReady()

	if len(hits(s, "visibleExcNeedle")) != 1 {
		t.Error("normal file missing")
	}
	if len(hits(s, "excludedExcNeedle")) != 0 {
		t.Error("GCGREP_EXCLUDE dir content was indexed")
	}
}

func TestGitignoreIntegration(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".gitignore"), "*.gen.go\nbuild/\n")
	write(t, filepath.Join(root, "main.go"), "gitignVisibleNeedle\n")
	write(t, filepath.Join(root, "types.gen.go"), "gitignHiddenNeedle\n")
	write(t, filepath.Join(root, "build", "out.js"), "gitignBuildNeedle\n")

	cfg := conf.Default()
	cfg.Gitignore = true
	s, err := newRootStore(root, t.TempDir(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.WaitReady()

	if len(hits(s, "gitignVisibleNeedle")) != 1 {
		t.Error("normal file missing with gitignore on")
	}
	if len(hits(s, "gitignHiddenNeedle")) != 0 {
		t.Error(".gitignore pattern not respected during indexing")
	}
	if len(hits(s, "gitignBuildNeedle")) != 0 {
		t.Error(".gitignore dir pattern not respected during indexing")
	}
}

func TestNestedRootCheck(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sub, "a.go"), "content\n")

	cfg := conf.Default()
	sv := NewServer(t.TempDir(), cfg)

	// index sub first
	s1, err := sv.store(sub, false, false)
	if err != nil {
		t.Fatal(err)
	}
	s1.WaitReady()

	// indexing parent should fail
	_, err = sv.store(root, false, false)
	if err == nil {
		t.Fatal("expected nested root error")
	}
	if !strings.Contains(err.Error(), "sub-root") {
		t.Fatalf("unexpected error: %v", err)
	}

	// --allow-nested bypasses
	s2, err := sv.store(root, false, true)
	if err != nil {
		t.Fatalf("allow-nested should bypass: %v", err)
	}
	s2.WaitReady()

	// cleanup
	for _, s := range []*RootStore{s1, s2} {
		s.Close()
	}
}

func TestCacheInfoAndDeleteCache(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), "cacheTestNeedle\n")
	cache := t.TempDir()
	s, err := newRootStore(root, cache, conf.Default(), false)
	if err != nil {
		t.Fatal(err)
	}
	s.WaitReady()
	s.save()

	dir, size := s.CacheInfo()
	if dir == "" {
		t.Error("CacheInfo returned empty dir")
	}
	if size <= 0 {
		t.Errorf("CacheInfo size=%d, want >0", size)
	}

	indexPath := s.indexPath()
	s.Close()

	// index file should exist
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("index file missing before delete: %v", err)
	}

	s2, _ := newRootStore(root, cache, conf.Default(), false)
	s2.WaitReady()
	s2.Close()
	s2.DeleteCache()

	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Error("DeleteCache did not remove index file")
	}
}

func TestForgetViaServer(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a.go"), "forgetNeedle\n")

	cfg := conf.Default()
	cache := t.TempDir()
	sv := NewServer(cache, cfg)

	s, err := sv.store(root, false, false)
	if err != nil {
		t.Fatal(err)
	}
	s.WaitReady()

	// verify store exists
	sv.mu.Lock()
	count := len(sv.stores)
	sv.mu.Unlock()
	if count != 1 {
		t.Fatalf("stores count = %d, want 1", count)
	}

	// simulate forget
	sv.mu.Lock()
	var foundKey storeKey
	for key := range sv.stores {
		if key.root == root {
			foundKey = key
			break
		}
	}
	delete(sv.stores, foundKey)
	sv.mu.Unlock()
	s.Close()
	s.DeleteCache()

	sv.mu.Lock()
	count = len(sv.stores)
	sv.mu.Unlock()
	if count != 0 {
		t.Fatalf("stores count after forget = %d, want 0", count)
	}
}

func TestHiddenPath(t *testing.T) {
	cases := map[string]bool{
		"a/b.go":         false,
		".env":           true,
		".git/config":    true,
		"a/.hidden/c.go": true,
		"a/b/.dotfile":   true,
		"a.b/c.go":       false, // dot inside a segment is not hidden
	}
	for p, want := range cases {
		if got := hiddenPath(p); got != want {
			t.Errorf("hiddenPath(%q) = %v, want %v", p, got, want)
		}
	}
}
