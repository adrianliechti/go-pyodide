// Package wasm embeds the CPython WASI build. Regenerate with
//
//	go run ./internal/build -version <cpython version> -sdk <wasi-sdk version>
package wasm

import (
	"embed"
	_ "embed"
	"io/fs"
	"strings"
)

// Module is the CPython interpreter compiled to wasm32-wasi.
//
//go:embed python.wasm
var Module []byte

// Version is the CPython version of Module, e.g. "3.14.7".
//
//go:embed version.txt
var version string

// Version returns the embedded CPython version, e.g. "3.14.7".
func Version() string { return strings.TrimSpace(version) }

//go:embed all:lib
var lib embed.FS

// Lib is the standard library tree, i.e. what the interpreter expects at
// <prefix>/lib. Sources are accompanied by hash-based .pyc files.
func Lib() fs.FS {
	sub, err := fs.Sub(lib, "lib")
	if err != nil {
		panic(err)
	}
	return sub
}
