//go:build windows

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
	name, err := syscall.UTF16PtrFromString(lockPath)
	if err != nil {
		return nil, err
	}
	// No sharing: a second process cannot open the lock until this handle closes.
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot lock journal %s (possibly in use): %w", path, err)
	}
	file := os.NewFile(uintptr(handle), lockPath)
	var once sync.Once
	var closeErr error
	return func() error {
		once.Do(func() { closeErr = file.Close() })
		return closeErr
	}, nil
}
