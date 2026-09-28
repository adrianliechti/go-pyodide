package pyodide

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"
)

func TestAESVectors(t *testing.T) {
	// NIST SP 800-38A, Appendix F: four blocks in ECB and CBC, every AES key size.
	data, err := os.ReadFile("testdata/aes.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []map[string]string
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	decode := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, v := range vectors {
		key, iv, plain := decode(v["key"]), decode(v["iv"]), decode(v["plain"])
		for _, mode := range []string{"ecb", "cbc"} {
			for _, operation := range []string{"encrypt", "decrypt"} {
				for _, chunk := range []int{1, 7, 16, 31, 64} {
					t.Run(fmt.Sprintf("%s/%s/%d/%d", mode, operation, len(key)*8, chunk), func(t *testing.T) {
						d, errno := openCryptoDevice([]string{"aes", mode, operation, fmt.Sprint(len(key))})
						if errno != 0 {
							t.Fatal(errno)
						}
						defer d.Close()
						setup := bytes.Clone(key)
						if mode == "cbc" {
							setup = append(setup, iv...)
						}
						for _, b := range setup {
							if n, errno := d.Write([]byte{b}); n != 1 || errno != 0 {
								t.Fatalf("setup: %d %v", n, errno)
							}
						}
						if errno := d.Datasync(); errno != 0 {
							t.Fatal(errno)
						}
						input, want := plain, decode(v[mode])
						if operation == "decrypt" {
							input, want = want, input
						}
						var got []byte
						for len(input) > 0 {
							part := input[:min(chunk, len(input))]
							if n, errno := d.Write(part); n != len(part) || errno != 0 {
								t.Fatalf("write: %d %v", n, errno)
							}
							input = input[len(part):]
							for {
								var buf [11]byte
								n, errno := d.Read(buf[:])
								if errno != 0 {
									t.Fatal(errno)
								}
								got = append(got, buf[:n]...)
								if n == 0 {
									break
								}
							}
						}
						if errno := d.Sync(); errno != 0 || !bytes.Equal(got, want) {
							t.Fatalf("finish %v: got %x, want %x", errno, got, want)
						}
						if flags := d.Status()[0]; flags != statusEOF {
							t.Fatalf("status: %d", flags)
						}
					})
				}
			}
		}
	}

	// Exercise the actual Python/WASI/device boundary with the same vectors.
	var stdout, stderr bytes.Buffer
	err = rt.Run(context.Background(), `
import json, sys
import _pyodide_crypto as crypto
for v in json.loads(sys.argv[1]):
    v = {k: bytes.fromhex(value) for k, value in v.items()}
    for mode in ("ecb", "cbc"):
        args = (v["key"],) if mode == "ecb" else (v["key"], v["iv"])
        encrypt = getattr(crypto, "aes_" + mode + "_encrypt")
        decrypt = getattr(crypto, "aes_" + mode + "_decrypt")
        assert encrypt(*args, v["plain"]) == v[mode]
        assert decrypt(*args, v[mode]) == v["plain"]
        assert encrypt(*args, b"") == decrypt(*args, b"") == b""
        # Multiple host chunks, including the last partial chunk.
        big = v["plain"] * 2049
        encrypted = encrypt(*args, big)
        assert decrypt(*args, encrypted) == big
        if mode == "ecb":
            assert encrypted == v[mode] * 2049
for args in ((b"short", b""), (b"k" * 16, b"unaligned")):
    try:
        crypto.aes_ecb_encrypt(*args)
    except ValueError:
        pass
    else:
        raise AssertionError("invalid AES input accepted")
try:
    crypto.aes_cbc_decrypt(b"k" * 16, b"bad IV", b"" )
except ValueError:
    pass
else:
    raise AssertionError("invalid IV accepted")
print("AES vectors ok")
`, RunOptions{Args: []string{string(data)}, Stdout: &stdout, Stderr: &stderr})
	if err != nil || stdout.String() != "AES vectors ok\n" {
		t.Fatalf("%v: %s%s", err, stdout.String(), stderr.String())
	}
}

func TestAESDeviceLimits(t *testing.T) {
	newDevice := func() device {
		d, errno := openCryptoDevice([]string{"aes", "cbc", "encrypt", "16"})
		if errno != 0 {
			t.Fatal(errno)
		}
		t.Cleanup(func() { d.Close() })
		return d
	}
	for _, args := range [][]string{nil, {"aes", "gcm", "encrypt", "16"}, {"aes", "cbc", "bad", "16"}, {"aes", "cbc", "encrypt", "15"}} {
		if _, errno := openCryptoDevice(args); errno == 0 {
			t.Fatalf("accepted path %v", args)
		}
	}
	d := newDevice()
	if d.Datasync() == 0 || d.Status()[0]&statusError == 0 {
		t.Fatal("accepted missing key/IV")
	}
	d = newDevice()
	if _, errno := d.Write(make([]byte, 33)); errno == 0 {
		t.Fatal("accepted oversized key/IV")
	}
	d = newDevice()
	if d.Sync() == 0 {
		t.Fatal("accepted uninitialized device")
	}
	d = newDevice()
	d.Write(make([]byte, 32))
	if errno := d.Datasync(); errno != 0 {
		t.Fatal(errno)
	}
	if n, errno := d.Write(make([]byte, aesBufferLimit)); n != aesBufferLimit || errno != 0 {
		t.Fatalf("write: %d %v", n, errno)
	}
	if n, errno := d.Write(make([]byte, 16)); n != 0 || errno != experimentalsys.EAGAIN {
		t.Fatalf("output was not bounded: %d %v", n, errno)
	}
	buf := make([]byte, aesBufferLimit)
	if n, errno := d.Read(buf); n != len(buf) || errno != 0 {
		t.Fatalf("read: %d %v", n, errno)
	}
	if n, errno := d.Write(make([]byte, aesBufferLimit+1)); n != 0 || errno != experimentalsys.EAGAIN {
		t.Fatalf("oversized write: %d %v", n, errno)
	}
	if _, errno := d.Write([]byte{1}); errno != 0 {
		t.Fatal(errno)
	}
	if d.Sync() == 0 || !strings.Contains(string(d.Status()[1:]), "multiple of 16") {
		t.Fatal("accepted a partial final block")
	}
	d.Close()
	if _, errno := d.Write(nil); errno == 0 {
		t.Fatal("write after close succeeded")
	}
	if _, errno := d.Read(buf); errno == 0 {
		t.Fatal("read after close succeeded")
	}
}

func TestEncryptedPDF(t *testing.T) {
	ctx := context.Background()
	r, err := New(ctx, WithCacheDir(filepath.Join(os.TempDir(), "go-pyodide-test-cache")))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(ctx)
	if err := r.AddWheel(ctx, "testdata/wheels/pypdf-6.19.0-py3-none-any.whl"); err != nil {
		t.Fatal(err)
	}
	code, err := os.ReadFile("testdata/pdf/check.py")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = r.Run(ctx, string(code), RunOptions{
		Mounts: []Mount{{Path: "/fixtures", FS: os.DirFS("testdata/pdf")}},
		Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil || stdout.String() != "encrypted PDFs ok\n" {
		t.Fatalf("%v: %s%s", err, stdout.String(), stderr.String())
	}
}
