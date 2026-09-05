// Package pyodide runs CPython from Go without cgo: the official CPython
// WASI build (https://github.com/brettcannon/cpython-wasi-build) is embedded
// in this package together with its precompiled standard library and executed
// with wazero.
//
// Every Run is a fresh interpreter process. It sees the standard library, the
// wheels added to the Runtime, and whatever directories or fs.FS values you
// mount; it has no network and no access to the host beyond that.
//
//	rt, _ := pyodide.New(ctx, pyodide.WithCacheDir(cacheDir))
//	defer rt.Close(ctx)
//
//	rt.AddWheel(ctx, "requests-2.32.3-py3-none-any.whl")
//
//	out, _ := rt.Output(ctx, "import requests; print(requests.__version__)")
//
// Only pure-Python wheels (platform tag "any") can be added: the interpreter
// is a single wasm module, so it cannot load native extension modules. The
// embedded build also lacks a few optional stdlib modules that need external
// C libraries: zlib, bz2, lzma, ssl, sqlite3, ctypes and the tkinter family.
package pyodide
