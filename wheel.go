package pyodide

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// AddWheel installs a pure-Python wheel so that later Runs can import it.
// The wheel is extracted into SiteDir and its modules are byte-compiled once
// inside the interpreter. Adding a wheel whose dist-info already exists in
// SiteDir is a no-op; adding another version of an installed distribution
// removes the files of the old one first. Dependencies are not resolved: add
// them too.
func (r *Runtime) AddWheel(ctx context.Context, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	return r.addWheel(ctx, filepath.Base(file), f, info.Size())
}

// AddWheelBytes is AddWheel for a wheel held in memory. name is the wheel's
// file name, which carries the compatibility tags, e.g.
// "requests-2.32.3-py3-none-any.whl".
func (r *Runtime) AddWheelBytes(ctx context.Context, name string, data []byte) error {
	return r.addWheel(ctx, name, bytes.NewReader(data), int64(len(data)))
}

// Packages lists the distributions installed in SiteDir as "name-version".
func (r *Runtime) Packages() ([]string, error) {
	entries, err := os.ReadDir(r.site)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".dist-info") {
			out = append(out, strings.TrimSuffix(e.Name(), ".dist-info"))
		}
	}
	sort.Strings(out)
	return out, nil
}

func (r *Runtime) addWheel(ctx context.Context, name string, ra io.ReaderAt, size int64) error {
	if r.isClosed() {
		return ErrClosed
	}
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return fmt.Errorf("pyodide: %s: %w", name, err)
	}

	distInfo, err := findDistInfo(zr)
	if err != nil {
		return fmt.Errorf("pyodide: %s: %w", name, err)
	}
	if err := checkPure(zr, distInfo, name); err != nil {
		return err
	}
	dataDir := strings.TrimSuffix(distInfo, ".dist-info") + ".data"

	r.siteMu.Lock()
	defer r.siteMu.Unlock()

	if _, err := os.Stat(filepath.Join(r.site, distInfo)); err == nil {
		return nil
	}
	if err := r.uninstallOthers(distInfo); err != nil {
		return fmt.Errorf("pyodide: %s: %w", name, err)
	}

	// Track top-level entries so only the new files are byte-compiled.
	roots := map[string]bool{}

	extract := func() error {
		for _, f := range zr.File {
			target, ok := wheelTarget(f.Name, dataDir)
			if !ok {
				continue
			}
			if f.FileInfo().IsDir() {
				continue
			}
			roots[strings.SplitN(target, "/", 2)[0]] = true

			dst := filepath.Join(r.site, filepath.FromSlash(target))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			w, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(w, rc)
			rc.Close()
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := extract(); err != nil {
		// Leave the install retryable: without dist-info it is not "present".
		_ = os.RemoveAll(filepath.Join(r.site, distInfo))
		return fmt.Errorf("pyodide: %s: %w", name, err)
	}

	return r.compile(ctx, roots)
}

// uninstallOthers removes other installed versions of the distribution that
// distInfo belongs to, using their RECORD files.
func (r *Runtime) uninstallOthers(distInfo string) error {
	want := distName(distInfo)
	entries, err := os.ReadDir(r.site)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), ".dist-info") || e.Name() == distInfo || distName(e.Name()) != want {
			continue
		}
		if err := r.uninstall(e.Name()); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) uninstall(distInfo string) error {
	record, err := os.ReadFile(filepath.Join(r.site, distInfo, "RECORD"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dirs := map[string]bool{}
	for _, line := range strings.Split(string(record), "\n") {
		file, _, _ := strings.Cut(line, ",")
		file = strings.Trim(strings.TrimSpace(file), "\"")
		if file == "" {
			continue
		}
		target, ok := wheelTarget(file, strings.TrimSuffix(distInfo, ".dist-info")+".data")
		if !ok {
			continue
		}
		p := filepath.Join(r.site, filepath.FromSlash(target))
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Bytecode written next to the source, and the source's directories.
		if strings.HasSuffix(p, ".py") {
			_ = os.RemoveAll(filepath.Join(filepath.Dir(p), "__pycache__"))
		}
		for d := filepath.Dir(p); d != r.site && strings.HasPrefix(d, r.site); d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	if err := os.RemoveAll(filepath.Join(r.site, distInfo)); err != nil {
		return err
	}
	// Remove directories that are now empty, deepest first.
	var list []string
	for d := range dirs {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, d := range list {
		_ = os.Remove(d) // fails if not empty, which is fine
	}
	return nil
}

// distName returns the normalized distribution name of a dist-info directory.
func distName(distInfo string) string {
	name, _, _ := strings.Cut(strings.TrimSuffix(distInfo, ".dist-info"), "-")
	return strings.ToLower(strings.NewReplacer("_", "-", ".", "-").Replace(name))
}

// compile byte-compiles the given top-level entries of the site directory
// with hash-based .pyc files, so that later Runs skip parsing. Failures to
// compile individual files are ignored: the sources still import.
func (r *Runtime) compile(ctx context.Context, roots map[string]bool) error {
	args := []string{"-m", "compileall", "-q", "-f", "--invalidation-mode", "unchecked-hash"}
	site := guestSite()
	n := 0
	for root := range roots {
		if strings.HasSuffix(root, ".dist-info") {
			continue
		}
		args = append(args, path.Join(site, root))
		n++
	}
	if n == 0 {
		return nil
	}
	err := r.exec(ctx, args, RunOptions{Env: map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}}, true)
	var exit *ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return err
}

// checkPure rejects wheels built for a specific platform. The tags in the
// WHEEL metadata are authoritative; the file name is only consulted when the
// metadata carries no tags.
func checkPure(zr *zip.Reader, distInfo, name string) error {
	var tags []string
	for _, f := range zr.File {
		if f.Name != distInfo+"/WHEEL" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("pyodide: %s: %w", name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return fmt.Errorf("pyodide: %s: %w", name, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if tag, ok := strings.CutPrefix(line, "Tag:"); ok {
				tags = append(tags, strings.TrimSpace(tag))
			}
		}
	}
	if len(tags) == 0 {
		base := strings.TrimSuffix(path.Base(name), ".whl")
		if parts := strings.Split(base, "-"); len(parts) >= 5 {
			tags = append(tags, strings.Join(parts[len(parts)-3:], "-"))
		}
	}
	for _, tag := range tags {
		if !strings.HasSuffix(tag, "-any") {
			return fmt.Errorf("%w: %s (tag %s)", ErrNotPureWheel, name, tag)
		}
	}
	return nil
}

func findDistInfo(zr *zip.Reader) (string, error) {
	for _, f := range zr.File {
		dir, file := path.Split(f.Name)
		if file == "WHEEL" && strings.HasSuffix(dir, ".dist-info/") && !strings.Contains(strings.TrimSuffix(dir, "/"), "/") {
			return strings.TrimSuffix(dir, "/"), nil
		}
	}
	return "", errors.New("no .dist-info/WHEEL entry")
}

// wheelTarget maps an archive entry to its path relative to site-packages.
// Entries under <dist>.data/{purelib,platlib} are relocated to the root; the
// other .data subtrees (scripts, headers, data) have no place in a sandbox
// and are dropped. Unsafe paths are dropped too.
func wheelTarget(name, dataDir string) (string, bool) {
	name = path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
		return "", false
	}
	if rest, ok := strings.CutPrefix(name, dataDir+"/"); ok {
		kind, sub, found := strings.Cut(rest, "/")
		if !found || (kind != "purelib" && kind != "platlib") {
			return "", false
		}
		return sub, sub != ""
	}
	return name, true
}
