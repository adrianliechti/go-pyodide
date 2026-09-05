package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// scriptWheels reads cafe.wheels.json beside cafe.py. Local wheel paths are
// relative to the manifest, so running the script from another directory works.
func scriptWheels(script string) ([]string, error) {
	manifest := strings.TrimSuffix(script, filepath.Ext(script)) + ".wheels.json"
	data, err := os.ReadFile(manifest)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var wheels []string
	if err := json.Unmarshal(data, &wheels); err != nil {
		return nil, fmt.Errorf("%s: %w", manifest, err)
	}
	for i, wheel := range wheels {
		if strings.TrimSpace(wheel) == "" {
			return nil, fmt.Errorf("%s: wheel %d is empty", manifest, i+1)
		}
		if !strings.Contains(wheel, "://") && !filepath.IsAbs(wheel) {
			wheels[i] = filepath.Join(filepath.Dir(manifest), wheel)
		}
	}
	return wheels, nil
}
