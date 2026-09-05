package pyodide

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// buildWheel makes a wheel with a generated RECORD, as real wheels have.
func buildWheel(distInfo string, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var record strings.Builder
	for _, name := range names {
		w, _ := zw.Create(name)
		w.Write([]byte(files[name]))
		record.WriteString(name + ",,\n")
	}
	record.WriteString(distInfo + "/RECORD,,\n")
	w, _ := zw.Create(distInfo + "/RECORD")
	w.Write([]byte(record.String()))
	zw.Close()
	return buf.Bytes()
}

func wheelMeta(name, version string) map[string]string {
	return map[string]string{
		name + "-" + version + ".dist-info/WHEEL":    "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		name + "-" + version + ".dist-info/METADATA": "Metadata-Version: 2.1\nName: " + name + "\nVersion: " + version + "\n",
	}
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func newTestRuntime(t *testing.T, opts ...Option) *Runtime {
	t.Helper()
	opts = append([]Option{WithCacheDir(filepath.Join(os.TempDir(), "go-pyodide-test-cache"))}, opts...)
	r, err := New(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(context.Background()) })
	return r
}

func TestWheelUpgrade(t *testing.T) {
	ctx := context.Background()
	site := t.TempDir()
	r := newTestRuntime(t, WithSiteDir(site))

	v1 := buildWheel("pkg-1.0.dist-info", merge(wheelMeta("pkg", "1.0"), map[string]string{
		"pkg/__init__.py": "VERSION = 1\n",
		"pkg/old.py":      "GONE = True\n",
		"pkg/sub/deep.py": "X = 1\n",
	}))
	v2 := buildWheel("pkg-2.0.dist-info", merge(wheelMeta("pkg", "2.0"), map[string]string{
		"pkg/__init__.py": "VERSION = 2\n",
		"pkg/new.py":      "NEW = True\n",
	}))

	if err := r.AddWheelBytes(ctx, "pkg-1.0-py3-none-any.whl", v1); err != nil {
		t.Fatal(err)
	}
	out, err := r.Output(ctx, "import pkg, pkg.old, pkg.sub.deep; print(pkg.VERSION)")
	if err != nil || out != "1\n" {
		t.Fatalf("v1: %q %v", out, err)
	}
	// Byte-compiled on install; the upgrade must remove that too.
	if _, err := os.Stat(filepath.Join(site, "pkg", "__pycache__")); err != nil {
		t.Fatal("expected __pycache__ after install")
	}

	if err := r.AddWheelBytes(ctx, "pkg-2.0-py3-none-any.whl", v2); err != nil {
		t.Fatal(err)
	}
	pkgs, _ := r.Packages()
	if len(pkgs) != 1 || pkgs[0] != "pkg-2.0" {
		t.Fatalf("packages after upgrade: %v", pkgs)
	}
	if _, err := os.Stat(filepath.Join(site, "pkg", "sub")); !os.IsNotExist(err) {
		t.Errorf("stale directory pkg/sub survived the upgrade")
	}
	out, err = r.Output(ctx, `
import pkg, pkg.new
print(pkg.VERSION)
try:
    import pkg.old
except ModuleNotFoundError:
    print("old gone")
`)
	if err != nil || out != "2\nold gone\n" {
		t.Fatalf("v2: %q %v", out, err)
	}

	// Downgrade works the same way, and dist names are normalized.
	if err := r.AddWheelBytes(ctx, "pkg-1.0-py3-none-any.whl", v1); err != nil {
		t.Fatal(err)
	}
	pkgs, _ = r.Packages()
	if len(pkgs) != 1 || pkgs[0] != "pkg-1.0" {
		t.Fatalf("packages after downgrade: %v", pkgs)
	}
}

func TestWheelNamespacePackages(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)

	a := buildWheel("ns_a-1.0.dist-info", merge(wheelMeta("ns_a", "1.0"), map[string]string{"ns/a.py": "A = 'a'\n"}))
	b := buildWheel("ns_b-1.0.dist-info", merge(wheelMeta("ns_b", "1.0"), map[string]string{"ns/b.py": "B = 'b'\n"}))
	for name, w := range map[string][]byte{"ns_a-1.0-py3-none-any.whl": a, "ns_b-1.0-py3-none-any.whl": b} {
		if err := r.AddWheelBytes(ctx, name, w); err != nil {
			t.Fatal(err)
		}
	}
	out, err := r.Output(ctx, "import ns.a, ns.b; print(ns.a.A + ns.b.B, list(ns.__path__)[0].split('/')[-1])")
	if err != nil || out != "ab ns\n" {
		t.Fatalf("%q %v", out, err)
	}

	// Removing one distribution must leave the shared namespace directory
	// intact for the other.
	a2 := buildWheel("ns_a-2.0.dist-info", merge(wheelMeta("ns_a", "2.0"), map[string]string{"ns/a.py": "A = 'A'\n"}))
	if err := r.AddWheelBytes(ctx, "ns_a-2.0-py3-none-any.whl", a2); err != nil {
		t.Fatal(err)
	}
	out, err = r.Output(ctx, "import ns.a, ns.b; print(ns.a.A + ns.b.B)")
	if err != nil || out != "Ab\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestWheelSyntaxError(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)

	w := buildWheel("broken-1.0.dist-info", merge(wheelMeta("broken", "1.0"), map[string]string{
		"broken/__init__.py": "",
		"broken/good.py":     "OK = True\n",
		"broken/bad.py":      "def (:\n",
	}))
	// Byte-compilation is best effort: the wheel installs anyway.
	if err := r.AddWheelBytes(ctx, "broken-1.0-py3-none-any.whl", w); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.SiteDir(), "broken", "__pycache__", "good.cpython-314.pyc")); err != nil {
		t.Errorf("good module not precompiled: %v", err)
	}
	out, err := r.Output(ctx, `
import broken.good
print(broken.good.OK)
try:
    import broken.bad
except SyntaxError:
    print("syntax error")
`)
	if err != nil || out != "True\nsyntax error\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestWheelPlatlib(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)

	// Root-Is-Purelib: false puts modules under <dist>.data/platlib; still
	// pure python as long as the tag says so.
	w := buildWheel("plat-1.0.dist-info", map[string]string{
		"plat-1.0.dist-info/WHEEL":            "Wheel-Version: 1.0\nRoot-Is-Purelib: false\nTag: py3-none-any\n",
		"plat-1.0.dist-info/METADATA":         "Name: plat\nVersion: 1.0\n",
		"plat-1.0.data/platlib/platmod.py":    "WHERE = 'platlib'\n",
		"plat-1.0.data/purelib/puremod.py":    "WHERE = 'purelib'\n",
		"plat-1.0.data/headers/plat.h":        "#define X\n",
		"plat-1.0.data/data/share/plat/x.txt": "ignored\n",
	})
	if err := r.AddWheelBytes(ctx, "plat-1.0-py3-none-any.whl", w); err != nil {
		t.Fatal(err)
	}
	out, err := r.Output(ctx, "import platmod, puremod; print(platmod.WHERE, puremod.WHERE)")
	if err != nil || out != "platlib purelib\n" {
		t.Fatalf("%q %v", out, err)
	}
	for _, stray := range []string{"plat.h", "share", "plat-1.0.data"} {
		if _, err := os.Stat(filepath.Join(r.SiteDir(), stray)); err == nil {
			t.Errorf("%s should not be installed", stray)
		}
	}
}

func TestWheelPersistence(t *testing.T) {
	ctx := context.Background()
	site := t.TempDir()
	w := buildWheel("keep-1.0.dist-info", merge(wheelMeta("keep", "1.0"), map[string]string{"keep.py": "print('kept')\n"}))

	r1 := newTestRuntime(t, WithSiteDir(site))
	if err := r1.AddWheelBytes(ctx, "keep-1.0-py3-none-any.whl", w); err != nil {
		t.Fatal(err)
	}
	r1.Close(ctx)
	if _, err := os.Stat(site); err != nil {
		t.Fatal("user-provided site dir must survive Close")
	}

	// A new runtime on the same site dir sees the package without AddWheel.
	r2 := newTestRuntime(t, WithSiteDir(site))
	out, err := r2.Output(ctx, "import keep")
	if err != nil || out != "kept\n" {
		t.Fatalf("%q %v", out, err)
	}
	// And re-adding is a no-op that does not touch the files.
	info1, _ := os.Stat(filepath.Join(site, "keep.py"))
	if err := r2.AddWheelBytes(ctx, "keep-1.0-py3-none-any.whl", w); err != nil {
		t.Fatal(err)
	}
	info2, _ := os.Stat(filepath.Join(site, "keep.py"))
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("re-add rewrote files")
	}
}

func TestWheelConcurrentAdd(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		w := buildWheel("conc-1.0.dist-info", merge(wheelMeta("conc", "1.0"), map[string]string{"conc.py": "N = 1\n"}))
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- r.AddWheelBytes(ctx, "conc-1.0-py3-none-any.whl", w)
		}()
		go func() {
			defer wg.Done()
			// Runs during installs must not break either.
			_, err := r.Output(ctx, "import sys; print(len(sys.path))")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	pkgs, _ := r.Packages()
	if len(pkgs) != 1 {
		t.Fatalf("packages: %v", pkgs)
	}
}

func TestWheelMalformed(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)

	if err := r.AddWheelBytes(ctx, "x-1.0-py3-none-any.whl", []byte("nope")); err == nil {
		t.Error("expected error for non-zip")
	}
	noInfo := makeWheel(map[string]string{"x.py": "pass\n"})
	if err := r.AddWheelBytes(ctx, "x-1.0-py3-none-any.whl", noInfo); err == nil || !strings.Contains(err.Error(), "dist-info") {
		t.Errorf("expected dist-info error, got %v", err)
	}
	if err := r.AddWheel(ctx, filepath.Join(t.TempDir(), "missing.whl")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
	// Nothing was installed by any of the above.
	if pkgs, _ := r.Packages(); len(pkgs) != 0 {
		t.Errorf("packages: %v", pkgs)
	}
	// Trailing whitespace and CRLF in WHEEL tags are tolerated.
	crlf := buildWheel("crlf-1.0.dist-info", map[string]string{
		"crlf-1.0.dist-info/WHEEL":    "Wheel-Version: 1.0\r\nTag: py2-none-any\r\nTag: py3-none-any \r\n",
		"crlf-1.0.dist-info/METADATA": "Name: crlf\r\nVersion: 1.0\r\n",
		"crlf.py":                     "V = 1\n",
	})
	if err := r.AddWheelBytes(ctx, "crlf-1.0-py2.py3-none-any.whl", crlf); err != nil {
		t.Fatal(err)
	}
}
