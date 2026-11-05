package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Shared pacing is enabled by the executable, not by unit-level constructors.
// Tests that need the gate construct it explicitly in an isolated directory.
var providerGatesEnabled atomic.Bool

func enableProviderGates() { providerGatesEnabled.Store(true) }

func runtimeProviderGate(key string, interval time.Duration) (*providerGate, error) {
	if !providerGatesEnabled.Load() {
		return nil, nil
	}
	return newProviderGate(key, interval)
}

type providerRateState struct {
	Version       int       `json:"version"`
	Provider      string    `json:"provider"`
	LastRequestAt time.Time `json:"last_request_at,omitempty"`
	NextRequestAt time.Time `json:"next_request_at,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
}

type providerGate struct {
	path     string
	key      string
	interval time.Duration
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

type providerLease struct {
	gate   *providerGate
	state  providerRateState
	unlock func() error
	mu     sync.Mutex
	closed bool
	err    error
}

// Construction performs no I/O. The executable can set HUNT_STATE_DIR to keep
// shared provider state in an explicitly selected persistent directory.
func newProviderGate(key string, interval time.Duration) (*providerGate, error) {
	switch key {
	case "github-graphql", "github-signup", "discord-eligibility":
	default:
		return nil, errors.New("unknown provider rate-state key")
	}
	if interval < 0 {
		return nil, errors.New("provider request interval must not be negative")
	}
	dir := os.Getenv("HUNT_STATE_DIR")
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return nil, errors.New("cannot locate provider state directory; set HUNT_STATE_DIR")
		}
		dir = filepath.Join(cache, "hunt", "providers")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.New("cannot resolve provider state directory")
	}
	return &providerGate{path: filepath.Join(abs, key+".json"), key: key, interval: interval}, nil
}

func (g *providerGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func (g *providerGate) pause(ctx context.Context, duration time.Duration) error {
	if g.sleep != nil {
		return g.sleep(ctx, duration)
	}
	return verificationPause(ctx, duration)
}

// Begin grants one request lease. Callers hold it through the response body and
// classification, persist any server deadline via Cooldown, then Close it.
// Result-journal timestamps migrate into the shared state conservatively.
func (g *providerGate) Begin(ctx context.Context, inheritedLast, inheritedCooldown time.Time) (*providerLease, error) {
	if g == nil {
		return nil, errors.New("provider gate is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0700); err != nil {
		return nil, fmt.Errorf("cannot create provider state directory: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unlock, err := lockJournal(g.path)
		if err != nil {
			if !providerLockBusy(err) {
				return nil, fmt.Errorf("cannot lock provider rate state: %w", err)
			}
			if err := g.pause(ctx, 25*time.Millisecond); err != nil {
				return nil, err
			}
			continue
		}
		state, err := g.read()
		if err != nil {
			unlock()
			return nil, err
		}
		changed := false
		if inheritedLast.After(state.LastRequestAt) {
			state.LastRequestAt = inheritedLast.UTC()
			changed = true
		}
		if inheritedCooldown.After(state.CooldownUntil) {
			state.CooldownUntil = inheritedCooldown.UTC()
			changed = true
		}
		if next := state.LastRequestAt.Add(g.interval); !state.LastRequestAt.IsZero() && next.After(state.NextRequestAt) {
			state.NextRequestAt = next.UTC()
			changed = true
		}
		now := g.clock()
		if now.Before(state.CooldownUntil) {
			if changed {
				err = g.write(state)
			}
			unlockErr := unlock()
			if err != nil || unlockErr != nil {
				return nil, errors.Join(err, unlockErr)
			}
			return nil, cooldownFailure(state.CooldownUntil, "shared provider cooldown")
		}
		if now.Before(state.NextRequestAt) {
			if changed {
				err = g.write(state)
			}
			unlockErr := unlock()
			if err != nil || unlockErr != nil {
				return nil, errors.Join(err, unlockErr)
			}
			if err := g.pause(ctx, state.NextRequestAt.Sub(now)); err != nil {
				return nil, err
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, err
		}
		state.LastRequestAt = now.UTC()
		state.NextRequestAt = now.Add(g.interval).UTC()
		if err := g.write(state); err != nil {
			unlock()
			return nil, err
		}
		return &providerLease{gate: g, state: state, unlock: unlock}, nil
	}
}

func providerLockBusy(err error) bool {
	if strings.HasPrefix(err.Error(), "journal is locked by another process:") {
		return true
	}
	// Windows opens the lock file without sharing. Distinguish contention from
	// access-denied and other failures, which must not become endless polling.
	return runtime.GOOS == "windows" && (errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(33)))
}

func (g *providerGate) read() (providerRateState, error) {
	state := providerRateState{Version: 1, Provider: g.key}
	file, err := os.Open(g.path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("cannot read provider rate state: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 32*1024+1))
	if err != nil || len(data) > 32*1024 {
		return state, errors.New("cannot read bounded provider rate state")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var saved providerRateState
	if decoder.Decode(&saved) != nil || saved.Version != 1 || saved.Provider != g.key {
		return providerRateState{}, errors.New("invalid provider rate state; no request was started")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return providerRateState{}, errors.New("invalid trailing provider rate state; no request was started")
	}
	return saved, nil
}

func (g *providerGate) write(state providerRateState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return errors.New("cannot encode provider rate state")
	}
	data = append(data, '\n')
	file, err := os.CreateTemp(filepath.Dir(g.path), ".hunt-rate-*")
	if err != nil {
		return fmt.Errorf("cannot create provider rate checkpoint: %w", err)
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if n, err := file.Write(data); err != nil || n != len(data) {
		file.Close()
		if err != nil {
			return err
		}
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("cannot sync provider rate checkpoint: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, g.path); err != nil {
		return fmt.Errorf("cannot replace provider rate checkpoint: %w", err)
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(g.path))
		if err != nil {
			return err
		}
		err = dir.Sync()
		closeErr := dir.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
	}
	return nil
}

func (l *providerLease) Cooldown(until time.Time) error {
	if l == nil {
		return errors.New("provider request lease is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("provider request lease is closed")
	}
	if !until.After(l.state.CooldownUntil) {
		return nil
	}
	l.state.CooldownUntil = until.UTC()
	return l.gate.write(l.state)
}

func (l *providerLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.err = l.unlock()
	}
	return l.err
}
