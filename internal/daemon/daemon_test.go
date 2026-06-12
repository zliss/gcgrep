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
	s, err := newRootStore(root, t.TempDir(), conf.Default())
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

	s1, err := newRootStore(root, cache, conf.Default())
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

	s2, err := newRootStore(root, cache, conf.Default())
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

	// per-root byte budget
	root2 := t.TempDir()
	half := strings.Repeat("y", 600<<10)
	for i := 0; i < 4; i++ {
		write(t, filepath.Join(root2, fmt.Sprintf("f%d.txt", i)), half)
	}
	cfg := conf.Default()
	cfg.MaxIndexMB = 1
	s2, err := newRootStore(root2, t.TempDir(), cfg)
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
}

func TestSkipCountersVisible(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "bin.dat"), "x\x00binary")
	write(t, filepath.Join(root, "utf16.txt"), "\xff\xfeh\x00i\x00") // BOM + NULs
	write(t, filepath.Join(root, "ok.go"), "plain text\n")
	s := newStore(t, root)
	defer s.Close()
	if got := s.skippedBinary.Load(); got != 2 {
		t.Errorf("skippedBinary = %d, want 2 (binary + utf16)", got)
	}
	if s.idx.NumFiles() != 1 {
		t.Errorf("NumFiles = %d, want 1", s.idx.NumFiles())
	}
}
