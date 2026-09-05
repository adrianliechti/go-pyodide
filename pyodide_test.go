package pyodide

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

var rt *Runtime

func TestMain(m *testing.M) {
	ctx := context.Background()
	cache := filepath.Join(os.TempDir(), "go-pyodide-test-cache")
	var err error
	rt, err = New(ctx, WithCacheDir(cache), WithEnv("GREETING", "hi"))
	if err != nil {
		panic(err)
	}
	code := m.Run()
	_ = rt.Close(ctx)
	os.Exit(code)
}

func TestVersion(t *testing.T) {
	out, err := rt.Output(context.Background(), "import sys; print(sys.version_info[:3])")
	if err != nil {
		t.Fatal(err)
	}
	want := "(" + strings.ReplaceAll(rt.Version(), ".", ", ") + ")\n"
	if out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestStdlib(t *testing.T) {
	out, err := rt.Output(context.Background(), `
import json, asyncio, dataclasses, decimal, hashlib, os, sys, sysconfig
assert sysconfig.get_paths()["purelib"] in sys.path, sys.path
print(json.dumps({"env": os.environ["GREETING"], "sha": hashlib.sha256(b"x").hexdigest()[:8]}))
print(asyncio.run(asyncio.sleep(0.01, result="slept")))
`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "{\"env\": \"hi\", \"sha\": \"2d711642\"}\nslept\n" {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestExitError(t *testing.T) {
	_, err := rt.Output(context.Background(), "raise ValueError('boom')")
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 1 || !strings.Contains(exit.Stderr, "ValueError: boom") {
		t.Fatalf("unexpected error: %v", err)
	}

	err = rt.Run(context.Background(), "import sys; sys.exit(7)", RunOptions{})
	if !errors.As(err, &exit) || exit.Code != 7 {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStdio(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := rt.Run(context.Background(), `
import sys
for line in sys.stdin:
    print(line.strip().upper())
print("warn", file=sys.stderr)
print(sys.argv[1:])
`, RunOptions{
		Args:   []string{"a", "b"},
		Stdin:  strings.NewReader("x\ny\n"),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "X\nY\n['a', 'b']\n" || stderr.String() != "warn\n" {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestMounts(t *testing.T) {
	work := t.TempDir()
	var stdout bytes.Buffer
	err := rt.Run(context.Background(), `
import pathlib
print(pathlib.Path("/data/cfg.txt").read_text())
pathlib.Path("/work/out.txt").write_text("written")
`, RunOptions{
		Stdout: &stdout,
		Mounts: []Mount{
			{Path: "/data", FS: fstest.MapFS{"cfg.txt": {Data: []byte("from fs.FS")}}},
			{Path: "/work", Dir: work},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "from fs.FS\n" {
		t.Fatalf("stdout %q", stdout.String())
	}
	got, err := os.ReadFile(filepath.Join(work, "out.txt"))
	if err != nil || string(got) != "written" {
		t.Fatalf("host file: %q %v", got, err)
	}

	// /tmp is writable and private to the run.
	out, err := rt.Output(context.Background(), `
import tempfile, os
with tempfile.NamedTemporaryFile("w", delete=False) as f: f.write("x")
print(os.listdir("/tmp"))
`)
	if err != nil || !strings.HasPrefix(out, "['tmp") {
		t.Fatalf("tempfile: %q %v", out, err)
	}

	// Read-only mounts reject writes.
	_, err = rt.Output(context.Background(), `open("/usr/local/lib/x", "w")`)
	var exit *ExitError
	if !errors.As(err, &exit) || !strings.Contains(exit.Stderr, "Error") {
		t.Fatalf("expected write to /lib to fail, got %v", err)
	}
}

func TestRunFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "helper.py"), []byte("def f(): return 'helped'\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.py"), []byte("import sys, helper\nprint(helper.f(), sys.argv)\n"), 0o644)

	var stdout bytes.Buffer
	err := rt.RunFile(context.Background(), filepath.Join(dir, "main.py"), RunOptions{Args: []string{"z"}, Stdout: &stdout})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "helped ['/script/main.py', 'z']\n" {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := rt.Run(ctx, "while True: pass", RunOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := rt.Output(context.Background(), "print(sum(range(1000)))")
			if err != nil || out != "499500\n" {
				t.Errorf("out %q err %v", out, err)
			}
		}()
	}
	wg.Wait()
}

// makeWheel builds a minimal PEP 427 wheel in memory.
func makeWheel(files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	zw.Close()
	return buf.Bytes()
}

func TestWheel(t *testing.T) {
	ctx := context.Background()
	site := t.TempDir()
	r, err := New(ctx, WithCacheDir(filepath.Join(os.TempDir(), "go-pyodide-test-cache")), WithSiteDir(site))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)

	wheel := makeWheel(map[string]string{
		"hello/__init__.py":               "from .impl import greet\n",
		"hello/impl.py":                   "def greet(): return 'hello from wheel'\n",
		"hello-1.0.data/purelib/extra.py": "VALUE = 42\n",
		"hello-1.0.data/scripts/hello":    "#!python\n",
		"hello-1.0.dist-info/WHEEL":       "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		"hello-1.0.dist-info/METADATA":    "Metadata-Version: 2.1\nName: hello\nVersion: 1.0\n",
		"../evil.py":                      "print('escaped')\n",
	})
	if err := r.AddWheelBytes(ctx, "hello-1.0-py3-none-any.whl", wheel); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	if err := r.AddWheelBytes(ctx, "hello-1.0-py3-none-any.whl", wheel); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(site, "hello", "__pycache__", "impl.cpython-314.pyc")); err != nil {
		t.Errorf("expected precompiled pyc: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(site), "evil.py")); err == nil {
		t.Error("path traversal entry was extracted")
	}
	if _, err := os.Stat(filepath.Join(site, "hello-1.0.data")); err == nil {
		t.Error(".data directory should not be extracted verbatim")
	}

	pkgs, err := r.Packages()
	if err != nil || len(pkgs) != 1 || pkgs[0] != "hello-1.0" {
		t.Fatalf("packages %v %v", pkgs, err)
	}

	out, err := r.Output(ctx, `
import hello, extra
from importlib.metadata import version
print(hello.greet(), extra.VALUE, version("hello"))
`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello from wheel 42 1.0\n" {
		t.Fatalf("out %q", out)
	}

	native := makeWheel(map[string]string{
		"native/__init__.py":            "",
		"native-1.0.dist-info/WHEEL":    "Wheel-Version: 1.0\nRoot-Is-Purelib: false\nTag: cp314-cp314-macosx_11_0_arm64\n",
		"native-1.0.dist-info/METADATA": "Name: native\nVersion: 1.0\n",
	})
	err = r.AddWheelBytes(ctx, "renamed.whl", native)
	if !errors.Is(err, ErrNotPureWheel) {
		t.Fatalf("expected ErrNotPureWheel, got %v", err)
	}
	if pkgs, _ := r.Packages(); len(pkgs) != 1 {
		t.Fatalf("rejected wheel must not be installed: %v", pkgs)
	}
}

func TestClosed(t *testing.T) {
	ctx := context.Background()
	r, err := New(ctx, WithCacheDir(filepath.Join(os.TempDir(), "go-pyodide-test-cache")))
	if err != nil {
		t.Fatal(err)
	}
	site := r.SiteDir()
	r.Close(ctx)
	if err := r.Run(ctx, "pass", RunOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(site); !os.IsNotExist(err) {
		t.Fatalf("temp site dir not removed: %v", err)
	}
}
