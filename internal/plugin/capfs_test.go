package plugin

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skromesch/mcplogview-go/internal/config"
)

// fsTool loads a plugin granted fs on roots and returns tool "x" running body.
func fsTool(t *testing.T, allow bool, roots []string, body string) fakeTool {
	t.Helper()
	ref := config.PluginRef{Name: "p", FS: &config.PluginFS{Roots: roots}}
	if allow {
		ref.Allow = []string{config.CapFS}
	}
	dir := t.TempDir()
	writePlugin(t, dir, "p", sshPlugin(body))
	h, s := hostWithSink(t, Options{}, plugins(dir, ref))
	if st := statusOf(t, h, "p"); !st.OK {
		t.Fatalf("load: %+v", st)
	}
	return s.get(t, "x")
}

func fsFixture(t *testing.T) (root, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "root-evil")
	for _, d := range []string{filepath.Join(root, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(root, "b.log"), []byte("0123456789"), 0o644))
	must(os.WriteFile(filepath.Join(root, "a.log"), []byte("hello"), 0o644))
	must(os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644))
	must(os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link-file")))
	must(os.Symlink(outside, filepath.Join(root, "link-dir")))
	must(os.Symlink(filepath.Join(root, "a.log"), filepath.Join(root, "link-ok")))
	return root, outside
}

func TestFSNotAvailableWithoutAllow(t *testing.T) {
	root, _ := fsFixture(t)
	tl := fsTool(t, false, []string{root}, `return { missing = mcp.fs == nil }`)
	m, err := callX(t, tl, nil)
	if err != nil || m["missing"] != true {
		t.Fatalf("%v %v", m, err)
	}
}

func TestFSListAndReadInsideRoot(t *testing.T) {
	root, _ := fsFixture(t)
	tl := fsTool(t, true, []string{root}, `
		local names = {}
		for _, e in ipairs(mcp.fs.list(a.dir)) do names[#names+1] = e.name .. ":" .. tostring(e.dir) end
		local r = mcp.fs.read(a.dir .. "/a.log")
		return { names = table.concat(names, ","), data = r.data, size = r.size }`)
	m, err := callX(t, tl, map[string]any{"dir": root})
	if err != nil {
		t.Fatal(err)
	}
	want := "a.log:false,b.log:false,link-dir:false,link-file:false,link-ok:false,sub:true"
	if m["names"] != want || m["data"] != "hello" || m["size"] != float64(5) {
		t.Fatalf("%v", m)
	}
}

func TestFSRejectsDotDotEscape(t *testing.T) {
	root, outside := fsFixture(t)
	tl := fsTool(t, true, []string{root}, `
		if a.list then return mcp.fs.list(a.p) end
		return mcp.fs.read(a.p)`)
	for _, p := range []string{
		root + "/../root-evil/secret", root + "/sub/../../root-evil/secret", outside + "/secret", outside,
	} {
		for _, list := range []bool{false, true} {
			if _, err := callX(t, tl, map[string]any{"p": p, "list": list}); err == nil {
				t.Errorf("path %q list=%v accepted", p, list)
			}
		}
	}
	// .. that stays inside is fine
	if _, err := callX(t, tl, map[string]any{"p": root + "/sub/../a.log"}); err != nil {
		t.Errorf("inside ..: %v", err)
	}
}

func TestFSRejectsSymlinkEscape(t *testing.T) {
	root, outside := fsFixture(t)
	tl := fsTool(t, true, []string{root}, `
		if a.list then return mcp.fs.list(a.p) end
		return mcp.fs.read(a.p)`)
	for _, c := range []struct {
		p    string
		list bool
	}{
		{root + "/link-file", false}, {root + "/link-dir/secret", false}, {root + "/link-dir", true},
	} {
		_, err := callX(t, tl, map[string]any{"p": c.p, "list": c.list})
		if err == nil {
			t.Errorf("%q accepted", c.p)
			continue
		}
		if !strings.Contains(err.Error(), c.p) || strings.Contains(err.Error(), outside) {
			t.Errorf("%q: error should name the argument, not the target: %v", c.p, err)
		}
	}
	if _, err := callX(t, tl, map[string]any{"p": root + "/link-ok"}); err != nil {
		t.Errorf("inside symlink: %v", err)
	}
}

func TestFSRejectsBadPaths(t *testing.T) {
	root, _ := fsFixture(t)
	tl := fsTool(t, true, []string{root}, `
		if a.list then return mcp.fs.list(a.p) end
		return mcp.fs.read(a.p)`)
	for _, c := range []struct {
		p    string
		list bool
	}{
		{"a.log", false}, {"", false}, {root + "/a.log\x00", false}, {root + "/missing", false},
		{root + "/sub", false}, {root + "/a.log", true}, {root + "/sub\x00", true},
	} {
		if _, err := callX(t, tl, map[string]any{"p": c.p, "list": c.list}); err == nil {
			t.Errorf("%q list=%v accepted", c.p, c.list)
		}
	}
}

func TestFSReadHonorsOffsetAndCap(t *testing.T) {
	root, _ := fsFixture(t)
	tl := fsTool(t, true, []string{root}, `
		local r = mcp.fs.read(a.dir .. "/b.log", a.off, a.max)
		return { data = r.data, size = r.size }`)
	for _, c := range []struct {
		off, max float64
		want     string
	}{{3, 4, "3456"}, {8, 100, "89"}, {10, 5, ""}, {20, 5, ""}, {0, 1, "0"}} {
		m, err := callX(t, tl, map[string]any{"dir": root, "off": c.off, "max": c.max})
		if err != nil || m["data"] != c.want || m["size"] != float64(10) {
			t.Errorf("off=%v max=%v: %v %v", c.off, c.max, m, err)
		}
	}
	for _, c := range [][2]float64{{-1, 5}, {0, 0}, {0, 1<<20 + 1}, {0.5, 1}} {
		_, err := callX(t, tl, map[string]any{"dir": root, "off": c[0], "max": c[1]})
		if err == nil {
			t.Errorf("off=%v max=%v accepted", c[0], c[1])
		}
	}
	// default max is 1 MiB
	big := filepath.Join(root, "big")
	if err := os.WriteFile(big, make([]byte, 1<<20+100), 0o644); err != nil {
		t.Fatal(err)
	}
	tl2 := fsTool(t, true, []string{root}, `local r = mcp.fs.read(a.p) return { n = #r.data, size = r.size }`)
	m, err := callX(t, tl2, map[string]any{"p": big})
	if err != nil || m["n"] != float64(1<<20) || m["size"] != float64(1<<20+100) {
		t.Fatalf("%v %v", m, err)
	}
}

func TestFSSymlinkToDirInsideRootIsDir(t *testing.T) {
	root, _ := fsFixture(t)
	if err := os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "link-sub")); err != nil {
		t.Fatal(err)
	}
	tl := fsTool(t, true, []string{root}, `
		for _, e in ipairs(mcp.fs.list(a.dir)) do if e.name == "link-sub" then return { dir = e.dir } end end`)
	m, err := callX(t, tl, map[string]any{"dir": root})
	if err != nil || m["dir"] != true {
		t.Fatalf("%v %v", m, err)
	}
}

func TestFSRootThatIsASymlink(t *testing.T) {
	root, _ := fsFixture(t)
	alias := filepath.Join(filepath.Dir(root), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	tl := fsTool(t, true, []string{alias}, `return { d = mcp.fs.read(a.p).data }`)
	for _, p := range []string{alias + "/a.log", root + "/a.log"} {
		m, err := callX(t, tl, map[string]any{"p": p})
		if err != nil || m["d"] != "hello" {
			t.Errorf("%s: %v %v", p, m, err)
		}
	}
}

func TestFSInsideHelperWithFilesystemRoot(t *testing.T) {
	c := newFSCap(config.PluginFS{Roots: []string{string(filepath.Separator)}})
	if !c.inside(string(filepath.Separator)) || !c.inside(filepath.Join(string(filepath.Separator), "x", "y")) {
		t.Fatal("filesystem root should contain everything")
	}
	c = newFSCap(config.PluginFS{Roots: []string{t.TempDir()}})
	if c.inside(string(filepath.Separator)) {
		t.Fatal("/ is not inside a sub-root")
	}
}

func TestFSNoExistenceOracle(t *testing.T) {
	root, _ := fsFixture(t)
	tl := fsTool(t, true, []string{root}, `return mcp.fs.read(a.p)`)
	_, e1 := callX(t, tl, map[string]any{"p": "/etc/passwd"})
	_, e2 := callX(t, tl, map[string]any{"p": "/etc/definitely-missing-xyz"})
	if e1 == nil || e2 == nil || !strings.Contains(e1.Error(), "outside the allowed roots") || !strings.Contains(e2.Error(), "outside the allowed roots") {
		t.Fatalf("%v / %v", e1, e2)
	}
	_, e3 := callX(t, tl, map[string]any{"p": root + "/missing"})
	if e3 == nil || !strings.Contains(e3.Error(), "no such file") {
		t.Fatalf("%v", e3)
	}
}

func TestFSSymlinkLoop(t *testing.T) {
	root, _ := fsFixture(t)
	if err := os.Symlink(filepath.Join(root, "loop2"), filepath.Join(root, "loop1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "loop1"), filepath.Join(root, "loop2")); err != nil {
		t.Fatal(err)
	}
	tl := fsTool(t, true, []string{root}, `return mcp.fs.read(a.p)`)
	_, err := callX(t, tl, map[string]any{"p": root + "/loop1"})
	if err == nil || !strings.Contains(err.Error(), "cannot access") {
		t.Fatalf("%v", err)
	}
}

func TestFSBadNumbers(t *testing.T) {
	root, _ := fsFixture(t)
	tl := fsTool(t, true, []string{root}, `return mcp.fs.read(a.dir .. "/b.log", a.off, a.max)`)
	nan, inf := math.NaN(), math.Inf(1)
	for _, c := range [][2]float64{
		{nan, 1}, {inf, 1}, {-inf, 1}, {0.5, 1}, {-1, 1}, {1e19, 1}, {math.Pow(2, 63), 1},
		{0, nan}, {0, inf}, {0, 0}, {0, 1<<20 + 1}, {0, 2.5}, {0, -3},
	} {
		if _, err := callX(t, tl, map[string]any{"dir": root, "off": c[0], "max": c[1]}); err == nil {
			t.Errorf("off=%v max=%v accepted", c[0], c[1])
		}
	}
}

func TestFSOffsetBeyondEOF(t *testing.T) {
	root, _ := fsFixture(t)
	tl := fsTool(t, true, []string{root}, `local r = mcp.fs.read(a.dir .. "/b.log", 1000, 5) return { n = #r.data, size = r.size }`)
	m, err := callX(t, tl, map[string]any{"dir": root})
	if err != nil || m["n"] != float64(0) || m["size"] != float64(10) {
		t.Fatalf("%v %v", m, err)
	}
}

func TestFSListTooLarge(t *testing.T) {
	root, _ := fsFixture(t)
	big := filepath.Join(root, "bigdir")
	if err := os.Mkdir(big, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxFSEntries; i++ {
		f, err := os.Create(filepath.Join(big, fmt.Sprintf("f%05d", i)))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	tl := fsTool(t, true, []string{root}, `return mcp.fs.list(a.p)`)
	_, err := callX(t, tl, map[string]any{"p": big})
	if err == nil || !strings.Contains(err.Error(), "directory too large (limit 10000 entries)") {
		t.Fatalf("%v", err)
	}
}
