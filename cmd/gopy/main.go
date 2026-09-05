// Command gopy runs a Python script with the embedded interpreter.
//
//	go run ./cmd/gopy script.py [args...]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	pyodide "github.com/adrianliechti/go-pyodide"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("gopy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var wheels []string
	flags.Func("wheel", "load a wheel from a local path or HTTP(S) URL (repeatable)", func(source string) error {
		wheels = append(wheels, source)
		return nil
	})
	out := flags.String("out", "", "mount a host directory read-write at /out (created if missing)")
	noAutoload := flags.Bool("no-autoload", false, "skip the script's .wheels.json manifest")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: gopy [options] script.py [args...]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() == 0 {
		flags.Usage()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	err := execute(ctx, flags.Args(), wheels, *out, !*noAutoload, stdin, stdout, stderr)
	if err == nil {
		return 0
	}
	var exit *pyodide.ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	if errors.Is(err, context.Canceled) {
		return 130
	}
	fmt.Fprintln(stderr, "gopy:", err)
	return 1
}

func execute(ctx context.Context, args, wheels []string, out string, autoload bool, stdin io.Reader, stdout, stderr io.Writer) error {
	info, err := os.Stat(args[0])
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s: expected a Python script, got a directory", args[0])
	}
	if autoload {
		auto, err := scriptWheels(args[0])
		if err != nil {
			return err
		}
		wheels = append(auto, wheels...)
	}

	var options []pyodide.Option
	if cache, err := os.UserCacheDir(); err == nil {
		options = append(options, pyodide.WithCacheDir(filepath.Join(cache, "go-pyodide")))
	}
	if out != "" {
		if err := os.MkdirAll(out, 0o755); err != nil {
			return err
		}
		options = append(options, pyodide.WithDir("/out", out))
	}
	rt, err := pyodide.New(ctx, options...)
	if err != nil {
		return err
	}
	defer rt.Close(context.Background())
	for _, wheel := range wheels {
		if strings.Contains(wheel, "://") {
			err = rt.AddWheelURL(ctx, wheel)
		} else {
			err = rt.AddWheel(ctx, wheel)
		}
		if err != nil {
			return err
		}
	}
	return rt.RunFile(ctx, args[0], pyodide.RunOptions{
		Args: args[1:], Stdin: stdin, Stdout: stdout, Stderr: stderr,
	})
}
