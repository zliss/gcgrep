package symbol

import (
	"fmt"
	"strings"
	"testing"
)

func find(defs []Def, name string) *Def {
	for i := range defs {
		if defs[i].Name == name {
			return &defs[i]
		}
	}
	return nil
}

func wantDef(t *testing.T, defs []Def, name string, kind Kind, container string) {
	t.Helper()
	d := find(defs, name)
	if d == nil {
		t.Fatalf("def %q not found in %+v", name, defs)
	}
	if d.Kind != kind || d.Container != container {
		t.Fatalf("def %q = kind %s container %q, want %s %q", name, d.Kind, d.Container, kind, container)
	}
}

func wantNoDef(t *testing.T, defs []Def, name string) {
	t.Helper()
	if d := find(defs, name); d != nil {
		t.Fatalf("unexpected def %q: %+v", name, *d)
	}
}

func TestGoExtract(t *testing.T) {
	src := `package p

type Server struct{}
type Handler interface{}
type ID = int

func NewServer() *Server { return nil }
func (s *Server) Start(port int) error { helper(); return nil }
func helper() {}
`
	defs := extractGo([]byte(src))
	wantDef(t, defs, "Server", KindStruct, "")
	wantDef(t, defs, "Handler", KindInterface, "")
	wantDef(t, defs, "NewServer", KindFunc, "")
	wantDef(t, defs, "Start", KindMethod, "Server")
	wantDef(t, defs, "helper", KindFunc, "")
	if d := find(defs, "Start"); d.Line != 8 {
		t.Fatalf("Start line = %d, want 8", d.Line)
	}
}

func TestJavaExtract(t *testing.T) {
	src := `package com.example;
// class FakeInComment {}
public class UserService extends Base {
    private static final String MODE = "class NotAClass {"; // string trap
    public UserService(Repo repo) { this.repo = repo; }
    public User getUser(long id) throws NotFoundException {
        if (cache(id)) { return fast(id); }
        return repo.find(id);
    }
    private boolean cache(long id) { return false; }
}
interface Repo {
    User find(long id);
}
enum Color { RED, GREEN }
`
	defs := extractJava([]byte(src))
	wantDef(t, defs, "UserService", KindClass, "")
	wantDef(t, defs, "getUser", KindMethod, "UserService")
	wantDef(t, defs, "cache", KindMethod, "UserService")
	wantDef(t, defs, "Repo", KindInterface, "")
	wantDef(t, defs, "find", KindMethod, "Repo") // abstract interface method
	wantDef(t, defs, "Color", KindEnum, "")
	wantNoDef(t, defs, "FakeInComment")
	wantNoDef(t, defs, "NotAClass")
	wantNoDef(t, defs, "fast") // call, not definition
	wantNoDef(t, defs, "if")
}

func TestPythonExtract(t *testing.T) {
	src := `# def commented(): pass
class UserService:
    def __init__(self, repo):
        self.repo = repo

    async def get_user(self, uid):
        return self.repo.find(uid)

def top_level(x):
    s = "def in_string(): pass"
    def inner():
        pass
    return inner
`
	defs := extractPython([]byte(src))
	wantDef(t, defs, "UserService", KindClass, "")
	wantDef(t, defs, "get_user", KindMethod, "UserService")
	wantDef(t, defs, "top_level", KindFunc, "")
	wantDef(t, defs, "inner", KindFunc, "top_level")
	wantNoDef(t, defs, "commented")
	wantNoDef(t, defs, "in_string")
}

func TestTSExtract(t *testing.T) {
	src := `// function fakeFn() {}
export class UserService {
  constructor(private repo: Repo) {}
  async getUser(id: number): Promise<User> {
    if (cached(id)) { return fast(id); }
    return this.repo.find(id);
  }
}
interface Repo { find(id: number): User }
enum Color { Red, Green }
type UserID = number
export function topLevel(x: string) { return x }
const arrowFn = async (a: number) => a + 1
const short = x => x * 2
`
	defs := extractTS([]byte(src))
	wantDef(t, defs, "UserService", KindClass, "")
	wantDef(t, defs, "getUser", KindMethod, "UserService")
	wantDef(t, defs, "constructor", KindMethod, "UserService")
	wantDef(t, defs, "Repo", KindInterface, "")
	wantDef(t, defs, "Color", KindEnum, "")
	wantDef(t, defs, "UserID", KindType, "")
	wantDef(t, defs, "topLevel", KindFunc, "")
	wantDef(t, defs, "arrowFn", KindFunc, "")
	wantDef(t, defs, "short", KindFunc, "")
	wantNoDef(t, defs, "fakeFn")
	wantNoDef(t, defs, "if")
	wantNoDef(t, defs, "cached") // call inside method body
}

func TestStripPreservesLayout(t *testing.T) {
	src := "a = 1 // c1\nb = \"str\" /* c2\nc2 */ c = 2\n"
	out := stripCLike([]byte(src))
	if len(out) != len(src) || strings.Count(string(out), "\n") != strings.Count(src, "\n") {
		t.Fatal("strip changed length or newlines")
	}
	if strings.Contains(string(out), "c1") || strings.Contains(string(out), "str") || strings.Contains(string(out), "c2") {
		t.Fatalf("strip left content: %q", out)
	}
	if !strings.Contains(string(out), "c = 2") {
		t.Fatalf("strip damaged code: %q", out)
	}
}

func TestRefs(t *testing.T) {
	src := `package p

func target() {}

func caller() {
	target()        // line 6: call
	x := target     // line 7: reference, not call
	// target() in comment: excluded
	s := "target()" // string: excluded
	_ = s
	_ = x
}
`
	refs := Refs("a.go", []byte(src), "target")
	if len(refs) != 2 {
		t.Fatalf("want 2 refs, got %+v", refs)
	}
	if refs[0].Line != 6 || !refs[0].IsCall {
		t.Fatalf("first ref should be call at line 6: %+v", refs[0])
	}
	if refs[1].Line != 7 || refs[1].IsCall {
		t.Fatalf("second ref should be non-call at line 7: %+v", refs[1])
	}
}

func TestRefsJavaNew(t *testing.T) {
	src := `class A {
    void m() {
        UserService s = new UserService(repo);
        other.getUser(1);
    }
}
`
	// decl type and `new` are on the same line: refs dedupe per line
	refs := Refs("A.java", []byte(src), "UserService")
	if len(refs) != 1 || refs[0].Line != 3 {
		t.Fatalf("want 1 ref on line 3, got %+v", refs)
	}
	got := Refs("A.java", []byte(src), "getUser")
	if len(got) != 1 || !got[0].IsCall {
		t.Fatalf("getUser call not found: %+v", got)
	}
}

func TestRefsScale(t *testing.T) {
	var b strings.Builder
	b.WriteString("package p\nfunc f() {\n")
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&b, "\tdoWork(%d)\n", i)
	}
	b.WriteString("}\n")
	refs := Refs("big.go", []byte(b.String()), "doWork")
	if len(refs) != 1000 {
		t.Fatalf("want 1000 refs, got %d", len(refs))
	}
}
