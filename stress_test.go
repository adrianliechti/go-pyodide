package pyodide

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func TestConcurrentHeavy(t *testing.T) {
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var stdout bytes.Buffer
			err := rt.Run(ctx, `
import sys, zlib, json, hashlib
n = int(sys.argv[1])
data = json.dumps({"i": n, "items": list(range(20000))}).encode()
c = zlib.compress(data, 9)
assert zlib.decompress(c) == data
print(n, hashlib.sha256(c).hexdigest()[:8], len(c) < len(data))
`, RunOptions{Args: []string{fmt.Sprint(i)}, Stdout: &stdout})
			if err != nil || !strings.HasPrefix(stdout.String(), fmt.Sprintf("%d ", i)) || !strings.HasSuffix(stdout.String(), "True\n") {
				t.Errorf("run %d: %q %v", i, stdout.String(), err)
			}
		}(i)
	}
	wg.Wait()
}

func TestMemoryLimit(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t, WithMemoryLimit(64<<20))
	out, err := r.Output(ctx, `
try:
    b = bytearray(200 << 20)
    print("allocated", len(b))
except MemoryError:
    print("MemoryError")
print("still alive")
`)
	if err != nil || out != "MemoryError\nstill alive\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestRecursion(t *testing.T) {
	ctx := context.Background()
	// The default limit is safe: RecursionError is an ordinary exception.
	out, err := rt.Output(ctx, `
def f(n): return f(n + 1)
try:
    f(0)
except RecursionError:
    print("caught")
`)
	if err != nil || out != "caught\n" {
		t.Fatalf("%q %v", out, err)
	}
	// Exhausting the wasm stack must surface as an error, not a panic, and
	// must not affect later runs.
	_, err = rt.Output(ctx, `
import sys
sys.setrecursionlimit(10_000_000)
def f(n): return f(n + 1)
try:
    f(0)
except RecursionError:
    pass
`)
	if err == nil {
		t.Log("wasm stack survived; fine")
	}
	out, err = rt.Output(ctx, "print('ok')")
	if err != nil || out != "ok\n" {
		t.Fatalf("runtime unusable after stack overflow: %q %v", out, err)
	}
}

func goroutinesSettle(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	buf := make([]byte, 1<<16)
	n := runtime.Stack(buf, true)
	t.Fatalf("goroutine leak: %d > %d\n%s", runtime.NumGoroutine(), want, buf[:n])
}

func TestCancelMidZlib(t *testing.T) {
	ctx := context.Background()
	before := runtime.NumGoroutine()
	for i := 0; i < 3; i++ {
		tctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		err := rt.Run(tctx, `
import zlib
c = zlib.compress(b"x" * 10_000_000)
d = zlib.decompressobj()
pos = 0
while True:  # decoder goroutine alive with buffered output when we are killed
    d.decompress(c[pos:pos + 10], 1)
    pos = (pos + 10) % (len(c) - 10)
`, RunOptions{})
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	}
	goroutinesSettle(t, before)
}

func TestCloseMidRun(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, "while True: pass", RunOptions{}) }()
	time.Sleep(200 * time.Millisecond)
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	select {
	case err := <-done:
		t.Logf("run returned %v after Close", time.Since(start))
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("run after Close returned %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("run did not return after Close")
	}
}

func TestLargeIO(t *testing.T) {
	ctx := context.Background()
	const size = 20 << 20
	in := bytes.Repeat([]byte("0123456789abcdef"), size/16)
	var stdout bytes.Buffer
	err := rt.Run(ctx, `
import sys
data = sys.stdin.buffer.read()
sys.stdout.buffer.write(data[::-1])
`, RunOptions{Stdin: bytes.NewReader(in), Stdout: &stdout})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != size || stdout.Bytes()[0] != 'f' || stdout.Bytes()[size-1] != '0' {
		t.Fatalf("got %d bytes", stdout.Len())
	}
}

func TestUnicode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var stdout bytes.Buffer
	err := rt.Run(ctx, `
import os, sys, pathlib
print(sys.argv[1], os.environ["GRÜSSE"], sys.getfilesystemencoding())
p = pathlib.Path("/w/日本語 файл.txt")
p.write_text("héllo wörld ✓", encoding="utf-8")
print(sorted(os.listdir("/w")), p.read_text(encoding="utf-8"))
`, RunOptions{
		Args:   []string{"ärger"},
		Env:    map[string]string{"GRÜSSE": "grüezi"},
		Stdout: &stdout,
		Mounts: []Mount{{Path: "/w", Dir: dir}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "ärger grüezi utf-8\n['日本語 файл.txt'] héllo wörld ✓\n"
	if stdout.String() != want {
		t.Fatalf("got %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "日本語 файл.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemOps(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	big := bytes.Repeat([]byte("abcdefgh"), 1<<20) // 8 MiB via fs.FS
	out, err := rt.Output(ctx, "pass")
	_ = out
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err = rt.Run(ctx, `
import os, shutil, hashlib, time
os.makedirs("/w/a/b/c")
with open("/w/a/b/c/f.txt", "w") as f: f.write("one\n")
with open("/w/a/b/c/f.txt", "a") as f: f.write("two\n")
os.rename("/w/a/b/c/f.txt", "/w/a/g.txt")
assert not os.path.exists("/w/a/b/c/f.txt")
st = os.stat("/w/a/g.txt")
assert st.st_size == 8 and abs(st.st_mtime - time.time()) < 60, st
shutil.copy("/w/a/g.txt", "/w/copy.txt")
os.remove("/w/a/g.txt")
os.rmdir("/w/a/b/c")
walk = [(root.replace("/w", ""), sorted(dirs), sorted(files)) for root, dirs, files in os.walk("/w")]
print(walk)
with open("/w/copy.txt", "rb") as f: print(f.read())
h = hashlib.sha256()
with open("/ro/big.bin", "rb") as f:
    while chunk := f.read(1 << 16): h.update(chunk)
print(h.hexdigest()[:16], os.path.getsize("/ro/big.bin"))
try:
    open("/ro/new", "w")
except OSError as e:
    print("ro:", e.errno)
`, RunOptions{
		Stdout: &stdout,
		Mounts: []Mount{
			{Path: "/w", Dir: dir},
			{Path: "/ro", FS: fstest.MapFS{"big.bin": {Data: big}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "[('', ['a'], ['copy.txt']), ('/a', ['b'], []), ('/a/b', [], [])]\nb'one\\ntwo\\n'\n"
	if !strings.HasPrefix(stdout.String(), want) || !strings.Contains(stdout.String(), " 8388608\nro: ") {
		t.Fatalf("got %q", stdout.String())
	}
}

func TestAsyncioPatched(t *testing.T) {
	out, err := rt.Output(context.Background(), `
import asyncio
async def main():
    loop = asyncio.get_running_loop()
    results = await asyncio.gather(asyncio.sleep(0.02, "a"), asyncio.sleep(0.01, "b"))
    fut = loop.create_future()
    loop.call_later(0.01, fut.set_result, "later")
    return results, await fut, loop._ssock
print(asyncio.run(main()))
`)
	if err != nil || out != "(['a', 'b'], 'later', None)\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestExitCodesAndPrecedence(t *testing.T) {
	ctx := context.Background()
	var stderr bytes.Buffer
	err := rt.Run(ctx, `import sys; sys.exit("bye")`, RunOptions{Stderr: &stderr})
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 1 || stderr.String() != "bye\n" {
		t.Fatalf("%v %q", err, stderr.String())
	}
	if err := rt.Run(ctx, `import sys; sys.exit(0)`, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Run(ctx, `import os; os._exit(9)`, RunOptions{}); !errors.As(err, &exit) || exit.Code != 9 {
		t.Fatalf("%v", err)
	}

	// Per-run env and mounts override runtime-level ones; runtime WithEnv
	// set GREETING=hi in TestMain.
	r := newTestRuntime(t, WithMount("/m", fstest.MapFS{"x": {Data: []byte("runtime")}}))
	out, err := r.Output(ctx, `import os; print(os.environ["GREETING"], open("/m/x").read())`)
	if err == nil {
		t.Fatal("GREETING must not leak between runtimes")
	}
	var stdout bytes.Buffer
	err = r.Run(ctx, `import os; print(os.environ.get("GREETING"), open("/m/x").read())`, RunOptions{
		Env:    map[string]string{"GREETING": "run"},
		Stdout: &stdout,
		Mounts: []Mount{{Path: "/m", FS: fstest.MapFS{"x": {Data: []byte("per-run")}}}},
	})
	if err != nil || stdout.String() != "run per-run\n" {
		t.Fatalf("%q %v", stdout.String(), err)
	}
	if _, err := r.Output(ctx, `import os; assert os.environ["PYTHONHOME"] == "/usr/local"`); err != nil {
		t.Fatal(err)
	}
	_ = out
}

func TestInvalidMounts(t *testing.T) {
	ctx := context.Background()
	for _, m := range []Mount{{Path: "rel", Dir: "."}, {Path: "/x"}, {Path: "/x", Dir: ".", FS: fstest.MapFS{}}} {
		if err := rt.Run(ctx, "pass", RunOptions{Mounts: []Mount{m}}); err == nil {
			t.Errorf("mount %+v accepted", m)
		}
	}
	if _, err := New(ctx, WithDir("nope", ".")); err == nil {
		t.Error("New accepted a relative mount path")
	}
}

func TestInterpreterMode(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	r := newTestRuntime(t, WithInterpreter())
	out, err := r.Output(context.Background(), "import json; print(json.dumps([1, 2]))")
	if err != nil || out != "[1, 2]\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func BenchmarkStartup(b *testing.B) {
	ctx := context.Background()
	for b.Loop() {
		if err := rt.Run(ctx, "pass", RunOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkImportJSON(b *testing.B) {
	ctx := context.Background()
	for b.Loop() {
		if err := rt.Run(ctx, "import json, dataclasses, typing", RunOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkZlib(b *testing.B) {
	ctx := context.Background()
	for b.Loop() {
		if err := rt.Run(ctx, "import zlib\nd = b'0123456789' * 1_000_000\nassert zlib.decompress(zlib.compress(d)) == d", RunOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}
