package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type synchronizedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *synchronizedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(data)
}

func sameFilePath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	a, errA := filepath.Abs(left)
	b, errB := filepath.Abs(right)
	if errA == nil && errB == nil && a == b {
		return true
	}
	ai, errA := os.Stat(left)
	bi, errB := os.Stat(right)
	return errA == nil && errB == nil && os.SameFile(ai, bi)
}

func runPipeline(ctx context.Context, cfg config, n, batch int, path, verifyPath string, find int, out, log io.Writer) error {
	if sameFilePath(path, verifyPath) {
		return errors.New("discovery and verification need different journal paths")
	}
	if sameFilePath(cfg.SourcePath, verifyPath) {
		return errors.New("verification journal must not overwrite the input wordlist")
	}
	session, err := openVerificationSession(verifyPath)
	if err != nil {
		return err
	}
	defer session.Close()
	session.force = cfg.Recheck
	return streamFind(ctx, find, out, log,
		func(scanCtx context.Context, enqueue func(string) error, safeLog io.Writer) error {
			return runDiscovery(scanCtx, cfg, n, batch, path, io.Discard, safeLog, enqueue)
		}, session.verify)
}

// One producer and one paced verifier overlap work. The bounded queue provides
// backpressure; discovery journals each candidate before it is queued.
func streamFind(ctx context.Context, find int, out, log io.Writer,
	produce func(context.Context, func(string) error, io.Writer) error,
	check func(context.Context, string) (verification, error)) error {
	scanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	names := make(chan string, 32)
	producerDone := make(chan error, 1)
	safeLog := &synchronizedWriter{w: log}
	go func() {
		err := produce(scanCtx, func(name string) error {
			select {
			case <-scanCtx.Done():
				return scanCtx.Err()
			case names <- name:
				return nil
			}
		}, safeLog)
		close(names)
		producerDone <- err
	}()
	finish := func(mainErr error) error {
		cancel()
		producerErr := <-producerDone
		if mainErr != nil {
			return mainErr
		}
		if producerErr != nil && !errors.Is(producerErr, context.Canceled) {
			return producerErr
		}
		return nil
	}
	found := 0
	for {
		select {
		case <-ctx.Done():
			return finish(ctx.Err())
		case name, ok := <-names:
			if !ok {
				fmt.Fprintf(safeLog, "Verification finished: %d available names.\n", found)
				return finish(nil)
			}
			result, err := check(scanCtx, name)
			if err != nil {
				return finish(fmt.Errorf("verify %s: %w", name, err))
			}
			fmt.Fprintf(safeLog, "%s: %s\n", name, result.Status)
			if result.Status == "available" {
				if _, err := fmt.Fprintln(out, name); err != nil {
					return finish(err)
				}
				found++
				if find > 0 && found >= find {
					fmt.Fprintf(safeLog, "Found %d available names; stopping. Pending candidates are saved for resume.\n", found)
					cancel()
					// A discovery error does not invalidate an independently verified
					// result after the requested number of finds has been reached.
					<-producerDone
					return nil
				}
			}
		}
	}
}
