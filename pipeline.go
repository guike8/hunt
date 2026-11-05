package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
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
	return runPipelineWithClaim(ctx, cfg, n, batch, path, verifyPath, find, out, log, nil)
}

func runPipelineWithClaim(ctx context.Context, cfg config, n, batch int, path, verifyPath string, find int, out, log io.Writer, claim func(context.Context, string) error) error {
	if claim != nil && find != 1 {
		return errors.New("GitHub rename mode requires exactly one verified name")
	}
	if sameFilePath(path, verifyPath) {
		return errors.New("discovery and verification need different journal paths")
	}
	if sameFilePath(cfg.SourcePath, verifyPath) {
		return errors.New("verification journal must not overwrite the input wordlist")
	}
	source, err := githubSource(cfg)
	if err != nil {
		return err
	}
	cfg.Source = source
	session, err := openVerificationSession(verifyPath)
	if err != nil {
		return err
	}
	defer session.Close()
	gate, err := runtimeProviderGate("github-signup", 7*time.Second)
	if err != nil {
		return err
	}
	session.verifier.gate = gate
	session.force = cfg.Recheck || claim != nil
	available := 0
	for _, result := range session.cache {
		if result.Status == "available" {
			available++
		}
	}
	progressVerification(ctx, len(session.cache), available)
	completed := make(map[string]bool)
	if cfg.ResumeVerification && !cfg.Recheck && claim == nil {
		// Snapshot before starting the producer. The verifier updates its cache
		// concurrently, so the producer only reads this immutable set.
		for name := range session.cache {
			if _, exists := session.recorded(name); exists && !session.needsPublish(name) {
				completed[name] = true
			}
		}
	}
	capacity := 32
	if find == 0 && claim == nil {
		capacity = max(1, source.Len())
	}
	return streamFindWithDelivery(ctx, find, out, log,
		func(scanCtx context.Context, enqueue func(string) error, safeLog io.Writer) error {
			queued := make(map[string]bool)
			enqueueOnce := func(name string) error {
				if completed[name] || queued[name] {
					return nil
				}
				if err := enqueue(name); err != nil {
					return err
				}
				queued[name] = true
				return nil
			}
			// Recover discovery inside its producer; verification can continue
			// draining saved candidates while the API waits through a cooldown.
			return runSearchWithWait(scanCtx, cfg.ResumeVerification && claim == nil, safeLog, func() error {
				return runDiscovery(scanCtx, cfg, n, batch, path, io.Discard, safeLog, enqueueOnce)
			})
		}, session.verify, claim, capacity, session.markPublished)
}

// One producer and one paced verifier overlap work. The bounded queue provides
// backpressure; discovery journals each candidate before it is queued.
func streamFind(ctx context.Context, find int, out, log io.Writer,
	produce func(context.Context, func(string) error, io.Writer) error,
	check func(context.Context, string) (verification, error)) error {
	return streamFindWithClaim(ctx, find, out, log, produce, check, nil)
}

func streamFindWithClaim(ctx context.Context, find int, out, log io.Writer,
	produce func(context.Context, func(string) error, io.Writer) error,
	check func(context.Context, string) (verification, error),
	claim func(context.Context, string) error) error {
	return streamFindWithClaimQueue(ctx, find, out, log, produce, check, claim, 32)
}

func streamFindWithClaimQueue(ctx context.Context, find int, out, log io.Writer,
	produce func(context.Context, func(string) error, io.Writer) error,
	check func(context.Context, string) (verification, error),
	claim func(context.Context, string) error, capacity int) error {
	return streamFindWithDelivery(ctx, find, out, log, produce, check, claim, capacity, nil)
}

func streamFindWithDelivery(ctx context.Context, find int, out, log io.Writer,
	produce func(context.Context, func(string) error, io.Writer) error,
	check func(context.Context, string) (verification, error),
	claim func(context.Context, string) error, capacity int,
	published func(verification) error) error {
	queue, err := newCandidateQueue(capacity)
	if err != nil {
		return err
	}
	if claim != nil && find != 1 {
		return errors.New("GitHub rename mode requires exactly one verified name")
	}
	scanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	producerDone := make(chan error, 1)
	producerFailed := make(chan struct{})
	safeLog := &synchronizedWriter{w: log}
	go func() {
		err := produce(scanCtx, func(name string) error {
			if err := queue.put(scanCtx, name); err != nil {
				return err
			}
			progressPending(ctx, queue.size())
			return nil
		}, safeLog)
		producerDone <- err
		queue.close()
		if err != nil && !errors.Is(err, context.Canceled) {
			close(producerFailed)
			cancel()
		}
	}()
	finish := func(mainErr error) error {
		cancel()
		producerErr := <-producerDone
		if mainErr != nil {
			if errors.Is(mainErr, context.Canceled) && ctx.Err() == nil && producerErr != nil && !errors.Is(producerErr, context.Canceled) {
				return producerErr
			}
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
		case <-producerFailed:
			return finish(nil)
		default:
		}
		name, ok, err := queue.take(scanCtx)
		if err != nil {
			return finish(err)
		}
		progressPending(ctx, queue.size())
		if !ok {
			fmt.Fprintf(safeLog, "Verification finished: %d available names.\n", found)
			return finish(nil)
		}
		result, err := check(scanCtx, name)
		if err != nil {
			return finish(fmt.Errorf("verify %s: %w", name, err))
		}
		if result.Name != name || (result.Status != "available" && result.Status != "unavailable") {
			return finish(errors.New("verification returned an inconclusive or mismatched result"))
		}
		fmt.Fprintf(safeLog, "%s: %s\n", name, result.Status)
		if result.Status == "available" {
			if claim != nil {
				if result.Name != name || !shortName.MatchString(name) {
					return finish(errors.New("refusing to claim a name without a matching three- or four-character verification"))
				}
				if err := ctx.Err(); err != nil {
					return finish(err)
				}
				// Stop discovery before the single account change. Keep the
				// caller context active so canceling the producer cannot cancel
				// confirmation of a rename that has already been submitted.
				cancel()
				if err := claim(ctx, name); err != nil {
					return finish(fmt.Errorf("claim %s: %w", name, err))
				}
				if err := publishResult(out, name); err != nil {
					return finish(fmt.Errorf("GitHub rename to %s succeeded, but writing its result failed: %w", name, err))
				}
				if published != nil {
					if err := published(result); err != nil {
						return finish(fmt.Errorf("GitHub rename succeeded, but recording its output failed: %w", err))
					}
				}
				fmt.Fprintf(safeLog, "GitHub rename confirmed: %s. Stopping after one account change.\n", name)
				<-producerDone
				return nil
			}
			if err := publishResult(out, name); err != nil {
				return finish(err)
			}
			if published != nil {
				if err := published(result); err != nil {
					return finish(err)
				}
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
