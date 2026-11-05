package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testProviderGate(t *testing.T, key string, interval time.Duration, at *time.Time) *providerGate {
	t.Helper()
	g, err := newProviderGate(key, interval)
	if err != nil {
		t.Fatal(err)
	}
	if at != nil {
		g.now = func() time.Time { return *at }
		g.sleep = func(ctx context.Context, delay time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			*at = at.Add(delay)
			return nil
		}
	}
	return g
}

func TestProviderGateCoordinatesIndependentInstances(t *testing.T) {
	t.Setenv("HUNT_STATE_DIR", t.TempDir())
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	first := testProviderGate(t, "github-signup", 7*time.Second, &now)
	second := testProviderGate(t, "github-signup", 7*time.Second, &now)
	lease, err := first.Begin(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	started := now
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err = second.Begin(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if now.Sub(started) != 7*time.Second {
		t.Fatalf("independent instance ignored request cadence: %s", now.Sub(started))
	}
	until := now.Add(1250250 * time.Microsecond)
	if err := lease.Cooldown(until); err != nil {
		t.Fatal(err)
	}
	// A short later observation must not shorten the shared server deadline.
	if err := lease.Cooldown(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	_, err = first.Begin(context.Background(), time.Time{}, time.Time{})
	var limit *searchCooldownError
	if !errors.As(err, &limit) || !limit.Until.Equal(until) {
		t.Fatalf("shared fractional cooldown lost: %v", err)
	}
}

func TestProviderGateMigratesJournalDeadlineBeforeNewRequest(t *testing.T) {
	t.Setenv("HUNT_STATE_DIR", t.TempDir())
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	g := testProviderGate(t, "discord-eligibility", time.Second, &now)
	until := now.Add(36*time.Minute + 500*time.Millisecond)
	_, err := g.Begin(context.Background(), now.Add(-time.Second), until)
	var limit *searchCooldownError
	if !errors.As(err, &limit) || !limit.Until.Equal(until) {
		t.Fatalf("inherited rate limit did not stop request: %v", err)
	}
	other := testProviderGate(t, "discord-eligibility", time.Second, &now)
	_, err = other.Begin(context.Background(), time.Time{}, time.Time{})
	if !errors.As(err, &limit) || !limit.Until.Equal(until) {
		t.Fatalf("different result journal could bypass migrated cooldown: %v", err)
	}
	now = until
	lease, err := other.Begin(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal("deadline should expire:", err)
	}
	lease.Close()
}

func TestProviderGateLeaseSerializesAndCancellationWorks(t *testing.T) {
	t.Setenv("HUNT_STATE_DIR", t.TempDir())
	g := testProviderGate(t, "github-graphql", 0, nil)
	lease, err := g.Begin(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	other := testProviderGate(t, "github-graphql", 0, nil)
	var polls atomic.Int32
	other.sleep = func(ctx context.Context, delay time.Duration) error {
		polls.Add(1)
		cancel()
		return ctx.Err()
	}
	_, err = other.Begin(ctx, time.Time{}, time.Time{})
	if !errors.Is(err, context.Canceled) || polls.Load() != 1 {
		t.Fatalf("lock waiter ignored cancellation or crossed lease: %v, polls %d", err, polls.Load())
	}
	lease.Close()
	if err := lease.Close(); err != nil {
		t.Fatal("lease Close is not idempotent:", err)
	}
	if err := lease.Cooldown(time.Now().Add(time.Hour)); err == nil {
		t.Fatal("closed lease mutated state")
	}
	acquired, err := other.Begin(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal("released lease stayed locked:", err)
	}
	acquired.Close()
}

func TestProviderGateFailsClosedOnCorruptState(t *testing.T) {
	for _, content := range []string{"", `{broken`, `{}`, `{"version":1,"provider":"wrong"}`, `{"version":1,"provider":"github-signup"} {}`, `{"version":1,"provider":"github-signup","unknown":true}`} {
		t.Run(content, func(t *testing.T) {
			t.Setenv("HUNT_STATE_DIR", t.TempDir())
			g := testProviderGate(t, "github-signup", time.Second, nil)
			if err := os.WriteFile(g.path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if lease, err := g.Begin(context.Background(), time.Time{}, time.Time{}); err == nil {
				lease.Close()
				t.Fatal("corrupt state permitted a request")
			}
			got, _ := os.ReadFile(g.path)
			if string(got) != content {
				t.Fatal("corrupt state was reset")
			}
		})
	}
}

func TestProviderGateKeepsRoutesSeparateAndWritesPrivateJSON(t *testing.T) {
	t.Setenv("HUNT_STATE_DIR", t.TempDir())
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	first := testProviderGate(t, "github-signup", time.Second, &now)
	lease, err := first.Begin(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	lease.Cooldown(now.Add(time.Hour))
	lease.Close()
	second := testProviderGate(t, "discord-eligibility", time.Second, &now)
	lease, err = second.Begin(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal("unrelated route inherited rate limit:", err)
	}
	lease.Close()
	data, err := os.ReadFile(first.path)
	if err != nil || !json.Valid(data) {
		t.Fatalf("checkpoint is not valid JSON: %v", err)
	}
	info, err := os.Stat(first.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 && os.PathSeparator != '\\' {
		t.Fatal("rate checkpoint is not private")
	}
	if _, err := newProviderGate("../escape", time.Second); err == nil {
		t.Fatal("arbitrary provider path accepted")
	}
}

func TestProviderRateChild(t *testing.T) {
	dir := os.Getenv("HUNT_RATE_TEST_CHILD_DIR")
	if dir == "" {
		return
	}
	os.Setenv("HUNT_STATE_DIR", dir)
	g, err := newProviderGate("github-graphql", time.Hour)
	if err != nil {
		os.Exit(2)
	}
	// Exit without Close to exercise OS release and checkpoint persistence.
	if _, err := g.Begin(context.Background(), time.Time{}, time.Time{}); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestProviderGateSurvivesProcessExit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HUNT_STATE_DIR", dir)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestProviderRateChild$")
	command.Env = append(os.Environ(), "HUNT_RATE_TEST_CHILD_DIR="+dir)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("child failed: %v %s", err, output)
	}
	g := testProviderGate(t, "github-graphql", time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	g.sleep = func(ctx context.Context, delay time.Duration) error {
		if delay < 59*time.Minute {
			t.Fatalf("process restart lost pacing deadline: %s", delay)
		}
		cancel()
		return ctx.Err()
	}
	_, err = g.Begin(ctx, time.Time{}, time.Time{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("exit left a stale lock or lost state: %v", err)
	}
	entries, _ := filepath.Glob(filepath.Join(dir, ".hunt-rate-*"))
	if len(entries) != 0 {
		t.Fatal("committed temporary checkpoint remains")
	}
}

func TestProviderGateConstructorsArePureAndRuntimeOptIn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	t.Setenv("HUNT_STATE_DIR", dir)
	previous := providerGatesEnabled.Load()
	providerGatesEnabled.Store(false)
	t.Cleanup(func() { providerGatesEnabled.Store(previous) })
	g, err := runtimeProviderGate("github-graphql", time.Second)
	if err != nil || g != nil {
		t.Fatal("unit-level constructor enabled shared state")
	}
	enableProviderGates()
	g, err = runtimeProviderGate("github-graphql", time.Second)
	if err != nil || g == nil || !strings.HasPrefix(g.path, dir+string(os.PathSeparator)) {
		t.Fatalf("runtime state override ignored: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("construction wrote state before a request")
	}
}
