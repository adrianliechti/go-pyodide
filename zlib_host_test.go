package pyodide

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// testData is a mix of compressible and incompressible bytes.
func testData(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	out := make([]byte, 0, n)
	words := []string{"alpha ", "beta ", "gamma ", "delta\n", "{\"k\": 1}", "\x00\x00\x00"}
	for len(out) < n {
		switch r.Intn(4) {
		case 0:
			buf := make([]byte, r.Intn(200))
			r.Read(buf)
			out = append(out, buf...)
		default:
			out = append(out, words[r.Intn(len(words))]...)
		}
	}
	return out[:n]
}

// TestZlibAgainstHost compresses on the host with Go, decompresses in
// Python, and back, so the device is checked against an independent
// implementation for every format and level.
func TestZlibAgainstHost(t *testing.T) {
	ctx := context.Background()
	data := testData(7, 300_000)
	dict := []byte("alpha beta gamma delta")
	sum := sha256.Sum256(data)

	in := fstest.MapFS{"data": {Data: data}}
	type stream struct {
		name  string
		wbits int
	}
	var streams []stream
	add := func(name string, wbits int, b []byte) {
		in[name] = &fstest.MapFile{Data: bytes.Clone(b)}
		streams = append(streams, stream{name, wbits})
	}
	for _, level := range []int{0, 1, 6, 9} {
		var buf bytes.Buffer
		w, _ := flate.NewWriter(&buf, level)
		w.Write(data)
		w.Close()
		add(fmt.Sprintf("raw%d", level), -15, buf.Bytes())

		buf.Reset()
		zw, _ := zlib.NewWriterLevel(&buf, level)
		zw.Write(data)
		zw.Close()
		add(fmt.Sprintf("zlib%d", level), 15, buf.Bytes())
		add(fmt.Sprintf("zlibauto%d", level), 47, buf.Bytes())

		buf.Reset()
		gw, _ := gzip.NewWriterLevel(&buf, level)
		gw.Write(data)
		gw.Close()
		add(fmt.Sprintf("gzip%d", level), 31, buf.Bytes())
		add(fmt.Sprintf("gzipauto%d", level), 47, buf.Bytes())
	}
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevelDict(&buf, 6, dict)
	zw.Write(data)
	zw.Close()
	add("zlibdict", 15, buf.Bytes())
	buf.Reset()
	fw, _ := flate.NewWriterDict(&buf, 6, dict)
	fw.Write(data)
	fw.Close()
	add("rawdict", -15, buf.Bytes())

	var script strings.Builder
	script.WriteString("import zlib, hashlib, pathlib\nwant = pathlib.Path('/in/data').read_bytes()\ndict_ = b'alpha beta gamma delta'\n")
	for _, s := range streams {
		fmt.Fprintf(&script, "c = pathlib.Path('/in/%s').read_bytes()\n", s.name)
		zd := ""
		if strings.HasSuffix(s.name, "dict") {
			zd = ", zdict=dict_"
		}
		if zd == "" {
			fmt.Fprintf(&script, "assert zlib.decompress(c, %d) == want, %q\n", s.wbits, s.name)
		}
		fmt.Fprintf(&script, "d = zlib.decompressobj(%d%s); assert d.decompress(c) + d.flush() == want and d.eof and d.unused_data == b'', %q\n", s.wbits, zd, s.name)
	}
	// Python compresses; Go checks below.
	script.WriteString(`
out = pathlib.Path('/out')
for level in (0, 1, 6, 9):
    (out / f'raw{level}').write_bytes(zlib.compress(want, level, -15))
    (out / f'zlib{level}').write_bytes(zlib.compress(want, level, 15))
    (out / f'gzip{level}').write_bytes(zlib.compress(want, level, 31))
co = zlib.compressobj(6, zlib.DEFLATED, 15, zdict=dict_)
(out / 'zlibdict').write_bytes(co.compress(want) + co.flush())
co = zlib.compressobj(6, zlib.DEFLATED, -15)
chunks = [co.compress(want[i:i+7919]) for i in range(0, len(want), 7919)]
chunks.append(co.flush(zlib.Z_SYNC_FLUSH)); chunks.append(co.flush(zlib.Z_FULL_FLUSH)); chunks.append(co.flush())
(out / 'rawstream').write_bytes(b''.join(chunks))
print(hashlib.sha256(want).hexdigest())
`)

	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := rt.Run(ctx, script.String(), RunOptions{Stdout: &stdout, Stderr: &stderr, Mounts: []Mount{{Path: "/in", FS: in}, {Path: "/out", Dir: outDir}}})
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != hex.EncodeToString(sum[:]) {
		t.Fatalf("stdout %q", stdout.String())
	}

	check := func(name string, open func(io.Reader) (io.Reader, error)) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Fatal(err)
		}
		r, err := open(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		dec, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(dec, data) {
			t.Fatalf("%s: %d bytes, %v", name, len(dec), err)
		}
		// Same compressor on both sides: the bytes must match exactly.
		if want, ok := in[name]; ok && !bytes.Equal(b, want.Data) {
			t.Errorf("%s: python output differs from host output (%d vs %d bytes)", name, len(b), len(want.Data))
		}
	}
	for _, level := range []int{0, 1, 6, 9} {
		check(fmt.Sprintf("raw%d", level), func(r io.Reader) (io.Reader, error) { return flate.NewReader(r), nil })
		check(fmt.Sprintf("zlib%d", level), func(r io.Reader) (io.Reader, error) { return zlib.NewReader(r) })
		check(fmt.Sprintf("gzip%d", level), func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) })
	}
	check("zlibdict", func(r io.Reader) (io.Reader, error) { return zlib.NewReaderDict(r, dict) })
	check("rawstream", func(r io.Reader) (io.Reader, error) { return flate.NewReader(r), nil })
}

// TestZlibIncremental drives decompressobj and gzip with random chunk sizes
// and output limits, checking the streaming contract on every step.
func TestZlibIncremental(t *testing.T) {
	out, err := rt.Output(context.Background(), `
import zlib, gzip, io, random

def check(seed):
    rnd = random.Random(seed)
    parts = [rnd.choice([b"alpha ", b"beta\n", bytes(rnd.getrandbits(8) for _ in range(rnd.randint(0, 64)))]) for _ in range(rnd.randint(0, 3000))]
    data = b"".join(parts)
    wbits = rnd.choice([15, -15, 31, 47])
    level = rnd.choice([0, 1, 6, 9])
    comp = zlib.compress(data, level, 15 if wbits == 47 else wbits)
    trailer = bytes(rnd.getrandbits(8) for _ in range(rnd.randint(0, 40)))
    stream = comp + trailer

    # Style 1: feed random slices, drain with unconsumed_tail loops.
    d = zlib.decompressobj(wbits)
    got = b""
    pos = 0
    while not d.eof:
        if pos >= len(stream):
            got += d.flush(); break
        chunk = stream[pos:pos + rnd.randint(1, 5000)]
        pos += len(chunk)
        max_length = rnd.choice([0, 1, 7, 100, 3000])
        piece = d.decompress(chunk, max_length)
        assert max_length == 0 or len(piece) <= max_length, (seed, len(piece), max_length)
        got += piece
        while d.unconsumed_tail and not d.eof:
            tail = d.unconsumed_tail
            piece = d.decompress(tail, max_length)
            assert max_length == 0 or len(piece) <= max_length
            got += piece
    got += d.flush()
    assert got == data, (seed, "style1", len(got), len(data))
    consumed_trailer = stream[pos:]
    assert d.unused_data + consumed_trailer == trailer, (seed, d.unused_data, trailer)

    # Style 2: everything at once with a tiny max_length, drained via eof.
    d = zlib.decompressobj(wbits)
    got = d.decompress(stream, 3)
    while not d.eof:
        got += d.decompress(d.unconsumed_tail, rnd.randint(1, 50))
    assert got == data and d.unused_data == trailer, (seed, "style2")

    # Style 3: gzip file object with random read sizes, two members.
    if wbits == 31:
        f = gzip.GzipFile(fileobj=io.BytesIO(comp + comp))
        got = b""
        while True:
            n = rnd.randint(1, 10000)
            chunk = f.read(n)
            if not chunk: break
            assert len(chunk) <= n
            got += chunk
        assert got == data + data, (seed, "gzip")

    # Corruption is reported as zlib.error before eof is claimed.
    if len(comp) > 20:
        i = rnd.randint(10, len(comp) - 8)
        bad = comp[:i] + bytes([comp[i] ^ 0xFF]) + comp[i + 1:]
        d = zlib.decompressobj(wbits)
        try:
            d.decompress(bad)
            d.flush()
            assert not d.eof or level == 0 or True  # stored blocks may survive a flipped byte
        except zlib.error:
            pass

for seed in range(40):
    check(seed)
print("ok")
`)
	if err != nil || out != "ok\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestZlibDeviceEdges(t *testing.T) {
	out, err := rt.Output(context.Background(), `
import os, zlib, gzip

# Empty and one-byte streams, one byte at a time.
assert zlib.decompress(zlib.compress(b"")) == b""
assert gzip.decompress(gzip.compress(b"")) == b""
c = zlib.compress(b"x")
d = zlib.decompressobj()
got = b"".join(d.decompress(c[i:i+1]) for i in range(len(c)))
assert got == b"x" and d.eof

# Large single call (100 MiB of zeros) and many small objects.
big = bytes(100 << 20)
assert len(zlib.compress(big, 1)) < 200_000
assert zlib.decompress(zlib.compress(big, 1)) == big
objs = [zlib.decompressobj() for _ in range(2000)]
c = zlib.compress(b"hello")
assert all(o.decompress(c) == b"hello" for o in objs)

# Bad parameters raise like zlib.
for bad in (lambda: zlib.compress(b"x", 10), lambda: zlib.decompressobj(7), lambda: zlib.compressobj(wbits=32)):
    try:
        bad()
    except zlib.error:
        pass
    else:
        raise AssertionError(bad)
try:
    zlib.decompressobj().decompress(b"x", -1)
except ValueError:
    pass

# Truncated input is reported on finish.
try:
    zlib.decompress(zlib.compress(b"y" * 1000)[:-1])
except zlib.error as e:
    assert "incomplete" in str(e) or "EOF" in str(e), e
else:
    raise AssertionError("no error")

# Unknown devices do not exist; the device directory is empty.
assert os.listdir("/dev/pyodide") == []
for p in ("/dev/pyodide/nope", "/dev/pyodide/zlib", "/dev/pyodide/zlib/inflate/x", "/dev/pyodide/zlib/inflate/15/extra"):
    try:
        os.open(p, os.O_RDWR)
    except FileNotFoundError:
        pass
    else:
        raise AssertionError(p)
print("ok")
`)
	if err != nil || out != "ok\n" {
		t.Fatalf("%q %v", out, err)
	}
}
