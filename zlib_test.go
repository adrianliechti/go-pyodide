package pyodide

import (
	"context"
	"strings"
	"testing"
)

func TestZlib(t *testing.T) {
	out, err := rt.Output(context.Background(), `
import zlib, gzip, zipfile, io, os, random

random.seed(1)
data = b"".join(random.choice([b"hello ", b"world ", b"pyodide "]) for _ in range(50000))

# one-shot, all formats
for wbits in (15, -15, 31):
    c = zlib.compress(data, 6, wbits)
    assert len(c) < len(data) // 10, (wbits, len(c))
    assert zlib.decompress(c, wbits) == data, wbits
assert zlib.decompress(zlib.compress(data), 47) == data
assert zlib.decompress(zlib.compress(data, wbits=31), 47) == data
assert zlib.decompress(zlib.compress(b""), 0) == b""

# streaming compression with sync flush
co = zlib.compressobj(9, zlib.DEFLATED, -15)
parts = [co.compress(data[:1000]), co.flush(zlib.Z_SYNC_FLUSH), co.compress(data[1000:]), co.flush()]
assert zlib.decompress(b"".join(parts), -15) == data

# decompressobj with max_length / unconsumed_tail / unused_data
c = zlib.compress(data) + b"trailing"
d = zlib.decompressobj()
out = b""
buf = c
while not d.eof:
    out += d.decompress(buf, 777)
    buf = d.unconsumed_tail
    assert len(out) <= len(data)
assert out == data, (len(out), len(data))
assert d.unused_data == b"trailing", d.unused_data
assert d.unconsumed_tail == b""
assert d.flush() == b""

# decompressobj without max_length, incremental feed
d = zlib.decompressobj(-15)
c = zlib.compress(data, 1, -15)
out = b"".join(d.decompress(c[i:i+100]) for i in range(0, len(c), 100)) + d.flush()
assert out == data and d.eof

# preset dictionary
zd = b"hello world pyodide "
c = zlib.compressobj(zdict=zd); cd = c.compress(data) + c.flush()
d = zlib.decompressobj(zdict=zd); assert d.decompress(cd) + d.flush() == data

# errors
for bad in (b"not compressed at all", zlib.compress(data)[:-5]):
    try:
        zlib.decompress(bad)
    except zlib.error as e:
        pass
    else:
        raise AssertionError("expected zlib.error")
# A truncated stream is not an error until the end is declared; Go's flate
# may hold back up to one 32 KiB window until then.
d = zlib.decompressobj()
partial = d.decompress(zlib.compress(data)[:-5])
assert data.startswith(partial) and len(partial) >= len(data) - 32768 and not d.eof, len(partial)

# checksums
assert zlib.crc32(b"hello") == 0x3610a686
assert zlib.adler32(b"hello") == 0x062c0215
assert zlib.adler32(b"world", zlib.adler32(b"hello ")) == zlib.adler32(b"hello world")

# gzip module (uses zlib._ZlibDecompressor)
g = gzip.compress(data)
assert gzip.decompress(g) == data
with gzip.GzipFile(fileobj=io.BytesIO(g + g)) as f:
    assert f.read() == data + data
buf = io.BytesIO()
with gzip.GzipFile(fileobj=buf, mode="wb") as f:
    for i in range(0, len(data), 4096): f.write(data[i:i+4096])
with gzip.GzipFile(fileobj=io.BytesIO(buf.getvalue())) as f:
    assert f.read(10) == data[:10] and f.read() == data[10:]

# zipfile
buf = io.BytesIO()
with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
    z.writestr("a.txt", data)
    z.writestr("b.txt", b"small")
with zipfile.ZipFile(io.BytesIO(buf.getvalue())) as z:
    assert z.read("a.txt") == data and z.read("b.txt") == b"small"
    with z.open("a.txt") as f:
        assert f.read(100) == data[:100]
        assert f.read() == data[100:]
    assert z.testzip() is None
print("zlib ok", len(data))
`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "zlib ok") {
		t.Fatalf("out %q", out)
	}
}
