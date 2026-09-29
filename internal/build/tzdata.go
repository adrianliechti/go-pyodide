package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Update these pins together from https://pypi.org/project/tzdata/#files,
// then run make fetch-tzdata. The wheel includes its licenses and metadata.
const (
	tzdataVersion = "2026.4"
	tzdataURL     = "https://files.pythonhosted.org/packages/f9/bc/8737e8d54cf51106118039b83f485a4783112fab49ea9d044b234978a46e/tzdata-2026.4-py2.py3-none-any.whl"
	tzdataSHA256  = "c2169a8b0a7a5e9674da5a135ccdfb2b3e671b333ed9fed17b41f73c34476e81"
)

func installTZData(ctx context.Context, out string) error {
	log.Printf("downloading tzdata %s", tzdataVersion)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tzdataURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tzdata download: %s", resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != tzdataSHA256 {
		return fmt.Errorf("tzdata SHA-256 mismatch: got %s, want %s", got, tzdataSHA256)
	}
	dir := filepath.Join(out, "lib")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "tzdata.whl"), data, 0o644)
}
