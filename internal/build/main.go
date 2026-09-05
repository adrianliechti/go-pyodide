// Command build downloads a CPython WASI release, precompiles its standard
// library to .pyc inside the wasm module itself and places the result in
// internal/wasm for embedding.
//
//	go run ./internal/build -version 3.14.7 -sdk 24
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

func main() {
	version := flag.String("version", "3.14.7", "CPython version")
	sdk := flag.String("sdk", "24", "wasi-sdk version used by the release")
	out := flag.String("out", "internal/wasm", "output directory")
	flag.Parse()

	if err := run(context.Background(), *version, *sdk, *out); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, version, sdk, out string) error {
	url := fmt.Sprintf("https://github.com/brettcannon/cpython-wasi-build/releases/download/v%s/python-%s-wasi_sdk-%s.zip", version, version, sdk)
	log.Printf("downloading %s", url)

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	archive, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return err
	}

	lib := filepath.Join(out, "lib")
	_ = os.RemoveAll(lib)
	_ = os.Remove(filepath.Join(out, "python.wasm"))

	var wasm []byte
	for _, f := range zr.File {
		name := filepath.Clean(f.Name)
		if strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("unsafe path in archive: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		switch {
		case name == "python.wasm":
			wasm = data
		case name == "LICENSE":
			// keep
		case strings.HasPrefix(name, "lib/"):
			// drop things that are useless without a terminal or a build
			// toolchain to keep the embedded tree small.
			if strings.Contains(name, "/__pycache__/") || strings.Contains(name, "/test/") || strings.Contains(name, "/tests/") {
				continue
			}
		default:
			continue
		}
		dst := filepath.Join(out, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	if wasm == nil {
		return fmt.Errorf("python.wasm not found in archive")
	}

	// Locate the stdlib directory (lib/python3.X).
	entries, err := os.ReadDir(lib)
	if err != nil {
		return err
	}
	var stdlib string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "python") {
			stdlib = e.Name()
		}
	}
	if stdlib == "" {
		return fmt.Errorf("stdlib directory not found under %s", lib)
	}

	// Install our additions (see patches/) into the stdlib directory.
	patches, err := os.ReadDir(filepath.Join(out, "patches"))
	if err != nil {
		return err
	}
	for _, p := range patches {
		data, err := os.ReadFile(filepath.Join(out, "patches", p.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(lib, stdlib, p.Name()), data, 0o644); err != nil {
			return err
		}
	}

	log.Printf("precompiling %s/%s", lib, stdlib)
	if err := precompile(ctx, wasm, lib, "/lib/"+stdlib); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(out, "version.txt"), []byte(version+"\n"), 0o644); err != nil {
		return err
	}
	log.Printf("done: python %s, stdlib %s", version, stdlib)
	return nil
}

// precompile runs `python -m compileall` inside the wasm module with lib
// mounted writable, producing hash-based .pyc files that do not depend on
// source mtimes (which an embedded fs.FS does not preserve).
func precompile(ctx context.Context, wasm []byte, lib, guestStdlib string) error {
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx)

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return err
	}
	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		return err
	}

	cfg := wazero.NewModuleConfig().
		WithName("").
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		WithRandSource(rand.Reader).
		WithSysWalltime().
		WithSysNanotime().
		WithEnv("PYTHONHOME", "/").
		// Otherwise the import system writes timestamp-based .pyc files for
		// every module compileall itself imports, which compileall then
		// leaves alone; embedded files have no reliable mtime.
		WithEnv("PYTHONDONTWRITEBYTECODE", "1").
		WithFSConfig(wazero.NewFSConfig().WithDirMount(lib, "/lib")).
		WithArgs("/python.wasm", "-m", "compileall", "-q", "-f", "--invalidation-mode", "unchecked-hash", guestStdlib)

	_, err = rt.InstantiateModule(ctx, compiled, cfg)
	return err
}
