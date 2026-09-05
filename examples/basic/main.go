// Command basic runs a few scripts: stdout capture, a read-only fs.FS mount,
// a read-write host directory, a traceback and a timeout.
//
//	go run ./examples/basic
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing/fstest"
	"time"

	pyodide "github.com/adrianliechti/go-pyodide"
)

func main() {
	ctx := context.Background()

	cache, _ := os.UserCacheDir()
	rt, err := pyodide.New(ctx,
		pyodide.WithCacheDir(filepath.Join(cache, "go-pyodide")),
		pyodide.WithMount("/config", fstest.MapFS{
			"settings.json": {Data: []byte(`{"greeting": "hello from an fs.FS"}`)},
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer rt.Close(ctx)

	fmt.Println("python", rt.Version())

	// Output collects stdout; the traceback of a failing script is on the error.
	out, err := rt.Output(ctx, `
import json, sys, platform
cfg = json.load(open("/config/settings.json"))
print(cfg["greeting"], "on", sys.platform, platform.machine())
`)
	fmt.Print(out)
	if err != nil {
		log.Fatal(err)
	}

	// A host directory mounted read-write, plus stdin and arguments.
	work, _ := os.MkdirTemp("", "pyodide-basic-")
	defer os.RemoveAll(work)

	err = rt.Run(ctx, `
import sys, csv, pathlib
rows = list(csv.reader(sys.stdin))
pathlib.Path("/work", sys.argv[1]).write_text(f"{len(rows)} rows\n")
`, pyodide.RunOptions{
		Args:   []string{"count.txt"},
		Stdin:  strings.NewReader("a,b\nc,d\ne,f\n"),
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Mounts: []pyodide.Mount{{Path: "/work", Dir: work}},
	})
	if err != nil {
		log.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(work, "count.txt"))
	fmt.Printf("count.txt: %s", data)

	// Errors are typed.
	_, err = rt.Output(ctx, `raise ValueError("boom")`)
	var exit *pyodide.ExitError
	if errors.As(err, &exit) {
		fmt.Printf("exit status %d, last line of stderr: %s\n", exit.Code, lastLine(exit.Stderr))
	}

	// A context deadline stops the interpreter.
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	err = rt.Run(tctx, `while True: pass`, pyodide.RunOptions{})
	fmt.Println("infinite loop:", err)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
