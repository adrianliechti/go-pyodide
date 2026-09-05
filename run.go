package pyodide

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental/sysfs"
	"github.com/tetratelabs/wazero/sys"

	"github.com/adrianliechti/go-pyodide/internal/wasm"
)

const (
	guestPrefix = "/usr/local"
	guestLib    = guestPrefix + "/lib"
	guestScript = "/script"
)

// guestSite is where the site directory is mounted. It is the interpreter's
// own site-packages path, so .pth files and sysconfig work as usual.
func guestSite() string {
	v := wasm.Version()
	if i := strings.LastIndex(v, "."); i > 0 {
		v = v[:i]
	}
	return path.Join(guestLib, "python"+v, "site-packages")
}

// RunOptions configures a single interpreter process. The zero value runs
// with no input and discards all output.
type RunOptions struct {
	// Args are the arguments after the program, i.e. sys.argv[1:].
	Args []string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Env adds or overrides environment variables for this run.
	Env map[string]string

	// Mounts are added to the mounts configured on the Runtime.
	Mounts []Mount
}

// Run executes code as with `python -c code`.
func (r *Runtime) Run(ctx context.Context, code string, opts RunOptions) error {
	return r.Exec(ctx, append([]string{"-c", code}, opts.Args...), opts)
}

// RunModule executes a module as with `python -m module`.
func (r *Runtime) RunModule(ctx context.Context, module string, opts RunOptions) error {
	return r.Exec(ctx, append([]string{"-m", module}, opts.Args...), opts)
}

// RunFile executes a Python script from the host filesystem. The script's
// directory is mounted read-only at /script so that imports of sibling
// modules work; sys.argv[0] is /script/<name>.
func (r *Runtime) RunFile(ctx context.Context, file string, opts RunOptions) error {
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return err
	}
	opts.Mounts = append(opts.Mounts, Mount{Path: guestScript, FS: os.DirFS(filepath.Dir(abs))})
	return r.Exec(ctx, append([]string{path.Join(guestScript, filepath.Base(abs))}, opts.Args...), opts)
}

// Output runs code and returns what it wrote to stdout. If the interpreter
// exits with a non-zero status, the returned *ExitError carries stderr, which
// is where the traceback went.
func (r *Runtime) Output(ctx context.Context, code string) (string, error) {
	var stdout, stderr bytes.Buffer
	err := r.Run(ctx, code, RunOptions{Stdout: &stdout, Stderr: &stderr})
	var exit *ExitError
	if errors.As(err, &exit) {
		exit.Stderr = stderr.String()
	}
	return stdout.String(), err
}

// Exec runs the interpreter with raw arguments, e.g. []string{"-m", "json.tool"}.
func (r *Runtime) Exec(ctx context.Context, args []string, opts RunOptions) error {
	return r.exec(ctx, args, opts, false)
}

func (r *Runtime) exec(ctx context.Context, args []string, opts RunOptions, siteWritable bool) error {
	if r.isClosed() {
		return ErrClosed
	}
	for _, m := range opts.Mounts {
		if err := m.validate(); err != nil {
			return err
		}
	}

	// Every run gets its own empty, writable /tmp so tempfile and the
	// libraries built on it work; a user mount at /tmp takes precedence.
	tmp, err := os.MkdirTemp("", "pyodide-tmp-")
	if err != nil {
		return fmt.Errorf("pyodide: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	fsc := wazero.NewFSConfig().WithFSMount(wasm.Lib(), guestLib)
	fsc = fsc.(sysfs.FSConfig).WithSysFSMount(devices, guestDev)
	fsc = fsc.WithDirMount(tmp, "/tmp")
	if siteWritable {
		fsc = fsc.WithDirMount(r.site, guestSite())
	} else {
		fsc = fsc.WithReadOnlyDirMount(r.site, guestSite())
	}
	for _, m := range r.mounts {
		fsc = mount(fsc, m)
	}
	for _, m := range opts.Mounts {
		fsc = mount(fsc, m)
	}

	env := map[string]string{
		"PYTHONHOME":       guestPrefix,
		"PYTHONUNBUFFERED": "1",
	}
	for k, v := range r.env {
		env[k] = v
	}
	for k, v := range opts.Env {
		env[k] = v
	}

	cfg := wazero.NewModuleConfig().
		WithName("").
		WithArgs(append([]string{"python"}, args...)...).
		WithFSConfig(fsc).
		WithRandSource(rand.Reader).
		WithSysWalltime().
		WithSysNanotime().
		WithSysNanosleep()
	for k, v := range env {
		cfg = cfg.WithEnv(k, v)
	}
	if opts.Stdin != nil {
		cfg = cfg.WithStdin(opts.Stdin)
	}
	if opts.Stdout != nil {
		cfg = cfg.WithStdout(opts.Stdout)
	}
	if opts.Stderr != nil {
		cfg = cfg.WithStderr(opts.Stderr)
	}

	mod, err := r.rt.InstantiateModule(ctx, r.compiled, cfg)
	if mod != nil {
		_ = mod.Close(ctx)
	}

	// Closing the runtime tears the module down with a clean exit status;
	// report that as ErrClosed rather than success.
	closed := r.isClosed()

	var exit *sys.ExitError
	switch {
	case err == nil:
		if closed {
			return ErrClosed
		}
		return nil
	case errors.As(err, &exit):
		switch exit.ExitCode() {
		case 0:
			if closed {
				return ErrClosed
			}
			return nil
		case sys.ExitCodeContextCanceled, sys.ExitCodeDeadlineExceeded:
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
		}
		return &ExitError{Code: int(exit.ExitCode())}
	default:
		return fmt.Errorf("pyodide: %w", err)
	}
}

func mount(fsc wazero.FSConfig, m Mount) wazero.FSConfig {
	if m.FS != nil {
		return fsc.WithFSMount(m.FS, m.Path)
	}
	return fsc.WithDirMount(m.Dir, m.Path)
}
