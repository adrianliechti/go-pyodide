package pyodide

import (
	"io/fs"
	"strings"
	"sync"

	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/tetratelabs/wazero/sys"
)

// guestDev is where host-backed device files live inside the interpreter.
const guestDev = "/dev/pyodide"

// device is a host-backed file: writes feed it data, reads drain its output,
// Datasync and Sync are its two control signals, Truncate passes a number and
// Pread returns a status record. Each open creates a fresh device; state
// never outlives the file.
type device interface {
	Write(p []byte) (int, experimentalsys.Errno)
	Read(p []byte) (int, experimentalsys.Errno)
	Status() []byte
	Truncate(size int64) experimentalsys.Errno
	Datasync() experimentalsys.Errno
	Sync() experimentalsys.Errno
	Close() experimentalsys.Errno
}

// deviceFS routes opens under guestDev to device constructors by the first
// path segment; the remaining segments are the device's parameters.
type deviceFS struct {
	experimentalsys.UnimplementedFS
	devices map[string]func(args []string) (device, experimentalsys.Errno)
}

var devices = &deviceFS{devices: map[string]func([]string) (device, experimentalsys.Errno){
	"zlib": openZlibDevice,
}}

func (d *deviceFS) OpenFile(path string, flag experimentalsys.Oflag, perm fs.FileMode) (experimentalsys.File, experimentalsys.Errno) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if parts[0] == "" || parts[0] == "." {
		// The mount root, opened by wazero at start-up.
		return &deviceDir{}, 0
	}
	open, ok := d.devices[parts[0]]
	if !ok {
		return nil, experimentalsys.ENOENT
	}
	dev, errno := open(parts[1:])
	if errno != 0 {
		return nil, errno
	}
	return &deviceFile{dev: dev}, 0
}

func (d *deviceFS) Stat(path string) (sys.Stat_t, experimentalsys.Errno) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if parts[0] == "" || parts[0] == "." {
		return sys.Stat_t{Mode: fs.ModeDir | 0o555, Nlink: 1}, 0
	}
	if _, ok := d.devices[parts[0]]; !ok {
		return sys.Stat_t{}, experimentalsys.ENOENT
	}
	return sys.Stat_t{Mode: fs.ModeCharDevice | 0o666, Nlink: 1}, 0
}

func (d *deviceFS) Lstat(path string) (sys.Stat_t, experimentalsys.Errno) {
	return d.Stat(path)
}

// deviceDir is the (empty) mount root.
type deviceDir struct {
	experimentalsys.UnimplementedFile
}

func (deviceDir) Stat() (sys.Stat_t, experimentalsys.Errno) {
	return sys.Stat_t{Mode: fs.ModeDir | 0o555, Nlink: 1}, 0
}

func (deviceDir) IsDir() (bool, experimentalsys.Errno) { return true, 0 }

func (deviceDir) Readdir(n int) ([]experimentalsys.Dirent, experimentalsys.Errno) {
	return nil, 0
}

type deviceFile struct {
	experimentalsys.UnimplementedFile
	mu  sync.Mutex
	dev device
}

func (f *deviceFile) Stat() (sys.Stat_t, experimentalsys.Errno) {
	return sys.Stat_t{Mode: fs.ModeCharDevice | 0o666, Nlink: 1}, 0
}

func (f *deviceFile) IsDir() (bool, experimentalsys.Errno) { return false, 0 }

func (f *deviceFile) Read(p []byte) (int, experimentalsys.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dev.Read(p)
}

func (f *deviceFile) Write(p []byte) (int, experimentalsys.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dev.Write(p)
}

// Pread returns the device's status record regardless of offset.
func (f *deviceFile) Pread(p []byte, off int64) (int, experimentalsys.Errno) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := f.dev.Status()
	if off >= int64(len(status)) {
		return 0, 0
	}
	return copy(p, status[off:]), 0
}

func (f *deviceFile) Truncate(size int64) experimentalsys.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dev.Truncate(size)
}

func (f *deviceFile) Datasync() experimentalsys.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dev.Datasync()
}

func (f *deviceFile) Sync() experimentalsys.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dev.Sync()
}

func (f *deviceFile) Close() experimentalsys.Errno {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dev == nil {
		return 0
	}
	errno := f.dev.Close()
	f.dev = nil
	return errno
}
