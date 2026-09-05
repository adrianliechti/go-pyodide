package pyodide

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// AddWheelURL downloads and installs a pure-Python wheel from an HTTP(S) URL.
// The URL path must end in the wheel's filename. An optional #sha256=<hex>
// fragment verifies the download before installation. Dependencies are not
// resolved and downloads are not cached. The host performs the request; the
// interpreter itself has no network access.
func (r *Runtime) AddWheelURL(ctx context.Context, source string) error {
	if r.isClosed() {
		return ErrClosed
	}
	u, err := url.Parse(source)
	if err != nil {
		return fmt.Errorf("pyodide: wheel URL: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("pyodide: wheel URL must use http or https")
	}
	name := path.Base(u.Path)
	if !strings.HasSuffix(name, ".whl") {
		return fmt.Errorf("pyodide: wheel URL path must end in a .whl filename")
	}
	var expected string
	if u.Fragment != "" {
		var ok bool
		expected, ok = strings.CutPrefix(u.Fragment, "sha256=")
		digest, err := hex.DecodeString(expected)
		if !ok || err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("pyodide: wheel URL fragment must be sha256=<64 hex digits>")
		}
		u.Fragment = ""
	}

	// Bound the whole download, while honoring shorter caller deadlines.
	downloadCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("pyodide: download %s: %w", name, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("pyodide: download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pyodide: download %s: %s", name, resp.Status)
	}

	f, err := os.CreateTemp("", "pyodide-wheel-*.whl")
	if err != nil {
		return fmt.Errorf("pyodide: download %s: %w", name, err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, digest), resp.Body)
	if err != nil {
		return fmt.Errorf("pyodide: download %s: %w", name, err)
	}
	if expected != "" && !strings.EqualFold(expected, hex.EncodeToString(digest.Sum(nil))) {
		return fmt.Errorf("pyodide: download %s: SHA-256 mismatch", name)
	}
	return r.addWheel(ctx, name, f, size)
}
