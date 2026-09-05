# go-pyodide

CPython embedded in Go, **without cgo and without native libraries**. The
official [CPython WASI build](https://github.com/brettcannon/cpython-wasi-build)
is compiled into the package together with its precompiled standard library
and executed with [wazero](https://wazero.io), a pure-Go wasm runtime. One
`go get`, any `GOOS/GOARCH` Go supports, `CGO_ENABLED=0`. Pure-Python wheels
can be added at runtime.

```bash
go get github.com/adrianliechti/go-pyodide
```

## Example

```go
package main

import (
	"context"
	"fmt"
	"log"

	pyodide "github.com/adrianliechti/go-pyodide"
)

func main() {
	ctx := context.Background()

	rt, err := pyodide.New(ctx, pyodide.WithCacheDir(".cache"))
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close(ctx)

	if err := rt.AddWheel(ctx, "tabulate-0.10.0-py3-none-any.whl"); err != nil {
		log.Fatal(err)
	}

	out, err := rt.Output(ctx, `
from tabulate import tabulate
print(tabulate([["go", 2009], ["python", 1991]], headers=["language", "year"]))
`)
	if err != nil {
		log.Fatal(err) // *pyodide.ExitError carries the traceback
	}
	fmt.Print(out)
}
```

[`examples/basic`](./examples/basic) shows stdin, arguments, mounts, typed
errors and timeouts. [`examples/packages`](./examples/packages) resolves a list
of packages against PyPI, downloads the pure wheels with their dependencies,
installs them into a persistent site directory and runs them; it also reports
which packages need native code and therefore cannot run.

```bash
go run ./examples/basic
go run ./examples/packages
```

## Run a Python script

[`cmd/gopy`](./cmd/gopy) runs a script with the embedded interpreter; no local
Python installation is needed.

```bash
go run ./cmd/gopy examples/script.py Alice
# Hello, Alice!
# Python 3.14.7 running on wasi

# Or build a standalone executable:
go build -o gopy ./cmd/gopy
./gopy script.py [args...]
```

Arguments after the script are passed through as `sys.argv[1:]`. Standard
input, output and error connect to the terminal, and the executable returns
Python's exit status. Ctrl-C cancels the interpreter. The compiled interpreter
is cached in `go-pyodide` under your OS user cache directory.

The script's directory is mounted read-only at `/script`, so sibling imports
work and `__file__` and `sys.argv[0]` refer to `/script/<filename>`. Read files
beside the script using `pathlib.Path(__file__).parent`; `/tmp` is writable
scratch space for the duration of the run.

### Café dashboard with wheels

[`examples/cafe.py`](./examples/cafe.py) generates café sales, prints a table
and a sparkline, and writes an Excel dashboard with two charts and filterable
data sheets. It uses [XlsxWriter](https://xlsxwriter.readthedocs.io/)
and [tabulate](https://pypi.org/project/tabulate/), both pure-Python wheels.

```bash
go run ./cmd/gopy -out ./out examples/cafe.py
# Creates ./out/cafe.xlsx

# Script arguments go after the script path:
go run ./cmd/gopy -out ./out examples/cafe.py --days 60 --seed 42
```

`gopy` automatically loads the wheels listed in
[`cafe.wheels.json`](./examples/cafe.wheels.json), next to the script. The
manifest is a JSON array of local wheel paths or HTTP(S) wheel URLs; local
paths are relative to the manifest. For `script.py`, name it
`script.wheels.json`. Each URL can include `#sha256=<64 hex digits>` to verify
the download before installation; the café manifest pins both URLs and hashes.

You can also pass wheels explicitly, repeating `-wheel` for each file or URL:

```bash
./gopy -wheel ./package-1.0-py3-none-any.whl script.py
./gopy -wheel https://example.org/package-1.0-py3-none-any.whl script.py
```

Go downloads and installs the wheels before starting the script. Downloads
and installed packages are temporary for each invocation; only interpreter
compilation is cached. Include all dependencies in the manifest or flags:
the loader does not resolve package names or transitive dependencies. Use
`-no-autoload` to skip the manifest, for example when supplying local wheels
for an offline run. Explicit `-wheel` options load after the manifest.

`-out ./out` creates that host directory and mounts it read-write at `/out`.
Files written there survive the run. Put all `gopy` options before the script
path; arguments after it belong to the script.

## API

| | |
|---|---|
| `New(ctx, opts...)` | Compiles the embedded interpreter once per process. `WithCacheDir` keeps the compiled module on disk (about 1.5 s cold vs. 300 ms cached), `WithSiteDir` makes installed wheels persistent, `WithWheels` installs at start-up, `WithMount` / `WithDir` / `WithEnv` apply to every run, `WithMemoryLimit`, `WithInterpreter`. |
| `rt.Run(ctx, code, RunOptions)` | `python -c code` in a fresh interpreter process. `RunOptions` has `Args`, `Stdin`, `Stdout`, `Stderr`, `Env` and per-run `Mounts`. |
| `rt.Output(ctx, code)` | `Run` with stdout returned as a string; on failure the `*ExitError` has the traceback in `Stderr`. |
| `rt.RunFile(ctx, path, RunOptions)` | Runs a host script; its directory is mounted read-only at `/script` so sibling imports work. |
| `rt.RunModule(ctx, name, RunOptions)`, `rt.Exec(ctx, args, RunOptions)` | `python -m name`, or raw interpreter arguments. |
| `rt.AddWheel(ctx, path)`, `rt.AddWheelBytes(ctx, name, data)` | Installs a pure-Python wheel into the site directory and byte-compiles it once. Not a package manager: dependencies are not resolved, add them too. |
| `rt.AddWheelURL(ctx, url)` | Downloads an HTTP(S) wheel on the host and installs it. Optional `#sha256=...` verification; context cancellation and a two-minute download timeout. |
| `rt.Packages()` | Installed distributions as `name-version`. |

Every run is a separate interpreter process with its own memory. It sees the
standard library, the site directory, an empty writable `/tmp`, and what you
mount: any `io/fs.FS` (an `embed.FS`, `os.DirFS`, `fstest.MapFS`, ...) read-only
with `WithMount` or `Mount{FS: ...}`, or a host directory read-write with
`WithDir` or `Mount{Dir: ...}`. The interpreter has no network access; Go can
download wheels before a run with `AddWheelURL`. A context deadline kills
the process and returns `context.DeadlineExceeded`. Runs can execute
concurrently.

```go
err := rt.Run(ctx, script, pyodide.RunOptions{
	Stdout: os.Stdout,
	Mounts: []pyodide.Mount{
		{Path: "/data", FS: os.DirFS("./data")},   // read-only
		{Path: "/out", Dir: "./out"},              // read-write
	},
})
```

## What runs, what does not

The interpreter is one wasm module, so it can only run **pure-Python
wheels** (platform tag `any`); `AddWheel` rejects the rest with
`ErrNotPureWheel`. There is no numpy, Pillow, lxml or cryptography, and no
package that imports them. Unlike Pyodide in the browser, which ships
Emscripten builds of those, nothing here fills that gap.

The WASI build has no threads, no subprocesses, no sockets and no OpenSSL:
`threading.Thread.start()`, `subprocess`, `socket.socket` and `ssl` fail.
`asyncio` works for coroutines and timers (a start-up patch removes the
event loop's socket-based wake-up).

The build also lacks the stdlib modules that depend on external C libraries:
bz2, lzma, sqlite3, ctypes, tkinter. **zlib is provided by this package**: the
Go `compress` packages serve a `zlib` module through a device file, so
`zipfile`, `gzip` and everything built on them (openpyxl, xlsxwriter, docx2txt,
pypdf, ...) work at native speed.

From the list in `examples/packages`: openpyxl, xlsxwriter, et-xmlfile,
docx2txt, pypdf, markdown, markdownify and tabulate run. seaborn (numpy),
python-docx and python-pptx (lxml), reportlab and pdfplumber (Pillow),
pdfminer.six (imports cryptography at module level) and extract-msg (imports
ssl) do not.

## Performance

Start-up is about 35 ms per run for a trivial script and 100–200 ms with
heavier imports such as asyncio, on an M-series Mac. The standard library and
every installed wheel are byte-compiled to hash-based `.pyc` files ahead of
time, so imports skip parsing. Compute-bound Python runs a few times slower
than native CPython. The embedded interpreter and standard library add about
50 MB to the binary.

## Testing

`make test` runs the suite in about 30 s: wheel edge cases (upgrades via
RECORD, namespace packages, `.data` relocation, concurrent installs), the
zlib device checked byte-for-byte against Go's own compressors and driven
with randomized chunk sizes and output limits, concurrency, memory caps,
stack exhaustion, cancellation mid-stream, closing during a run, 20 MB
stdio, Unicode paths and filesystem operations. `go test -race` passes too,
just slower. `make bench` reports start-up and zlib throughput.

## How it works

```
Go ── wazero ──▶ python.wasm (CPython 3.14, wasm32-wasi)
       │             ├─ /usr/local/lib   embedded stdlib (fs.FS)
       │             ├─ .../site-packages  wheels, extracted on the host
       │             ├─ /tmp             per-run scratch directory
       │             ├─ /dev/pyodide     host devices (zlib)
       │             └─ your mounts
       └── WASI: stdio, clock, random, filesystem
```

`internal/build` downloads a release, strips it, precompiles the standard
library inside the wasm module itself (so the `.pyc` format always matches)
and installs the files in `internal/wasm/patches` into it. To move to another
CPython version, change `PYTHON_VERSION` in the Makefile and run
`make fetch-python`; the same command must be run after editing a patch file,
because the precompiled `.pyc` files do not track source changes.

## License

MIT. The embedded interpreter is CPython, distributed under the PSF license;
see `internal/wasm/LICENSE`.
