//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package main

import "errors"

func lockJournal(path string) (func() error, error) {
	return nil, errors.New("journal locking is supported on macOS, Linux, BSD, and Windows")
}
