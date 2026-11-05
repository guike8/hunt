//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

func lockJournal(path string) (func() error, error) {
	lockPath, err := journalLockPath(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("journal is locked by another process: %s", path)
	}
	var once sync.Once
	var closeErr error
	return func() error {
		once.Do(func() { closeErr = file.Close() })
		return closeErr
	}, nil
}
