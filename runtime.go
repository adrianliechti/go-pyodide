package pyodide

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/adrianliechti/go-pyodide/internal/wasm"
)

// Runtime holds the compiled interpreter and the site directory that wheels
// are installed into. It is safe for concurrent use: Runs execute in
// independent interpreter instances.
type Runtime struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule

	site     string // host directory mounted as site-packages
	siteTemp bool   // site was created by us and is removed on Close

	mounts []Mount
	env    map[string]string

	siteMu sync.Mutex // serializes wheel installs

	mu     sync.Mutex
	closed bool
}

type config struct {
	cacheDir    string
	interpreter bool
	memoryLimit uint32 // pages
	site        string
	wheels      []string
	mounts      []Mount
	env         map[string]string
}

// Option configures New.
type Option func(*config)

// WithCacheDir caches the compiled module on disk so later New calls in other
// processes skip compilation, which takes a few seconds for the 30 MB module.
// The directory is created if missing.
func WithCacheDir(dir string) Option {
	return func(c *config) { c.cacheDir = dir }
}

// WithInterpreter forces wazero's interpreter instead of its native compiler.
// Much slower, but works on every platform Go supports.
func WithInterpreter() Option {
	return func(c *config) { c.interpreter = true }
}

// WithMemoryLimit caps each interpreter's linear memory in bytes (rounded up
// to 64 KiB pages, at most 4 GiB).
func WithMemoryLimit(bytes uint64) Option {
	return func(c *config) {
		pages := (bytes + 65535) / 65536
		if pages > 65536 {
			pages = 65536
		}
		c.memoryLimit = uint32(pages)
	}
}

// WithSiteDir uses dir as the site-packages directory that AddWheel installs
// into, so wheels survive across processes. Wheels whose dist-info is already
// present are not extracted again. Without this option a temporary directory
// is used and removed on Close.
func WithSiteDir(dir string) Option {
	return func(c *config) { c.site = dir }
}

// WithWheels adds wheel files at construction time; see AddWheel.
func WithWheels(paths ...string) Option {
	return func(c *config) { c.wheels = append(c.wheels, paths...) }
}

// WithMount mounts fsys read-only at guestPath for every Run. fsys can be
// anything implementing fs.FS: an embed.FS, os.DirFS, fstest.MapFS, ...
func WithMount(guestPath string, fsys fs.FS) Option {
	return func(c *config) { c.mounts = append(c.mounts, Mount{Path: guestPath, FS: fsys}) }
}

// WithDir mounts the host directory hostDir read-write at guestPath for every
// Run.
func WithDir(guestPath, hostDir string) Option {
	return func(c *config) { c.mounts = append(c.mounts, Mount{Path: guestPath, Dir: hostDir}) }
}

// WithEnv sets an environment variable for every Run.
func WithEnv(key, value string) Option {
	return func(c *config) {
		if c.env == nil {
			c.env = map[string]string{}
		}
		c.env[key] = value
	}
}

// New compiles the embedded interpreter.
func New(ctx context.Context, opts ...Option) (*Runtime, error) {
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}
	for _, m := range cfg.mounts {
		if err := m.validate(); err != nil {
			return nil, err
		}
	}

	var rcfg wazero.RuntimeConfig
	if cfg.interpreter {
		rcfg = wazero.NewRuntimeConfigInterpreter()
	} else {
		rcfg = wazero.NewRuntimeConfig()
	}
	rcfg = rcfg.WithCloseOnContextDone(true)
	if cfg.memoryLimit > 0 {
		rcfg = rcfg.WithMemoryLimitPages(cfg.memoryLimit)
	}
	if cfg.cacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(cfg.cacheDir)
		if err != nil {
			return nil, fmt.Errorf("pyodide: compilation cache: %w", err)
		}
		rcfg = rcfg.WithCompilationCache(cache)
	}

	rt := wazero.NewRuntimeWithConfig(ctx, rcfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("pyodide: instantiate wasi: %w", err)
	}
	compiled, err := rt.CompileModule(ctx, wasm.Module)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("pyodide: compile module: %w", err)
	}

	r := &Runtime{
		rt:       rt,
		compiled: compiled,
		mounts:   cfg.mounts,
		env:      cfg.env,
	}

	if cfg.site != "" {
		if err := os.MkdirAll(cfg.site, 0o755); err != nil {
			_ = rt.Close(ctx)
			return nil, fmt.Errorf("pyodide: site dir: %w", err)
		}
		r.site = cfg.site
	} else {
		dir, err := os.MkdirTemp("", "pyodide-site-")
		if err != nil {
			_ = rt.Close(ctx)
			return nil, fmt.Errorf("pyodide: site dir: %w", err)
		}
		r.site = dir
		r.siteTemp = true
	}

	for _, w := range cfg.wheels {
		if err := r.AddWheel(ctx, w); err != nil {
			_ = r.Close(ctx)
			return nil, err
		}
	}
	return r, nil
}

// Version returns the embedded CPython version, e.g. "3.14.7".
func (r *Runtime) Version() string { return wasm.Version() }

// SiteDir returns the host directory wheels are installed into. It is mounted
// as the interpreter's site-packages directory.
func (r *Runtime) SiteDir() string { return r.site }

// Close releases the compiled module and removes the temporary site directory,
// if any. Runs in progress are interrupted.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()

	err := r.rt.Close(ctx)
	if r.siteTemp {
		if rerr := os.RemoveAll(r.site); err == nil {
			err = rerr
		}
	}
	return err
}

func (r *Runtime) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

// Mount describes a directory visible to the interpreter. Exactly one of FS
// (read-only) or Dir (a host directory, read-write) must be set.
type Mount struct {
	// Path is the absolute path inside the interpreter, e.g. "/data".
	Path string
	// FS is mounted read-only.
	FS fs.FS
	// Dir is a host directory mounted read-write.
	Dir string
}

func (m Mount) validate() error {
	if m.Path == "" || !path.IsAbs(m.Path) {
		return fmt.Errorf("pyodide: mount path %q must be absolute", m.Path)
	}
	if (m.FS == nil) == (m.Dir == "") {
		return fmt.Errorf("pyodide: mount %s: exactly one of FS or Dir must be set", m.Path)
	}
	return nil
}
