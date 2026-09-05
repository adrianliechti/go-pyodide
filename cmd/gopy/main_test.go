package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunWheels(t *testing.T) {
	var wheel bytes.Buffer
	zw := zip.NewWriter(&wheel)
	for name, content := range map[string]string{
		"greeting.py":                  "MESSAGE = 'hello'\n",
		"greeting-1.0.dist-info/WHEEL": "Wheel-Version: 1.0\nTag: py3-none-any\n",
	} {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write(wheel.Bytes())
	}))
	defer server.Close()

	dir := t.TempDir()
	script := filepath.Join(dir, "script.py")
	writeFile(t, script, []byte(`import sys
from pathlib import Path
from greeting import MESSAGE
assert sys.argv[1:] == ['--script-flag', 'hello world']
Path('/out/result.txt').write_text(MESSAGE + ' ' + sys.stdin.read())
print('done')
print('diagnostic', file=sys.stderr)
`))
	local := filepath.Join(dir, "greeting-1.0-py3-none-any.whl")
	writeFile(t, local, wheel.Bytes())
	manifest := filepath.Join(dir, "script.wheels.json")
	for _, tc := range []struct {
		name, source string
		autoload     bool
	}{
		{"URL manifest", server.URL + "/greeting-1.0-py3-none-any.whl", true},
		{"relative local manifest", filepath.Base(local), true},
		{"explicit URL", server.URL + "/greeting-1.0-py3-none-any.whl", false},
		{"explicit local wheel", local, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "output")
			args := []string{"-out", out}
			if tc.autoload {
				data, err := json.Marshal([]string{tc.source})
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, manifest, data)
			} else {
				// Disabling autoload must skip even an invalid manifest.
				writeFile(t, manifest, []byte("invalid JSON"))
				args = append(args, "-no-autoload", "-wheel", tc.source)
			}
			args = append(args, script, "--script-flag", "hello world")
			var stdout, stderr bytes.Buffer
			if code := run(args, strings.NewReader("from stdin"), &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, &stderr)
			}
			if stdout.String() != "done\n" || stderr.String() != "diagnostic\n" {
				t.Fatalf("stdout %q, stderr %q", &stdout, &stderr)
			}
			if data, err := os.ReadFile(filepath.Join(out, "result.txt")); err != nil || string(data) != "hello from stdin" {
				t.Fatalf("output file: %q, %v", data, err)
			}
		})
	}

	writeFile(t, manifest, []byte("invalid JSON"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{script}, nil, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "script.wheels.json") || stdout.Len() != 0 {
		t.Fatalf("invalid manifest: exit %d, stdout %q, stderr %q", code, &stdout, &stderr)
	}
}

func TestRunExitStatus(t *testing.T) {
	script := filepath.Join(t.TempDir(), "exit.py")
	writeFile(t, script, []byte("import sys; sys.exit(7)\n"))
	var stdout, stderr bytes.Buffer
	if code := run([]string{script}, nil, &stdout, &stderr); code != 7 || stderr.Len() != 0 {
		t.Fatalf("exit %d, stderr %q", code, &stderr)
	}
}
