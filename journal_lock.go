package main

import (
	"fmt"
	"path/filepath"
)

// Lock files remain on disk; only the operating-system lock indicates ownership.
// Keeping the inode avoids races between an unlock and another process opening it.
func journalLockPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved + ".lock", nil
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("journal directory: %w", err)
	}
	return filepath.Join(dir, filepath.Base(abs)) + ".lock", nil
}
