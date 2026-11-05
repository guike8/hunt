package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestSearchWaitResumesOnlyKnownCooldown(t *testing.T) {
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	start := now
	calls, sleeps := 0, 0
	var log bytes.Buffer
	err := runSearchWithWaitClock(context.Background(), true, &log, func() error {
		calls++
		if calls == 1 {
			return fmt.Errorf("provider: %w", cooldownFailure(start.Add(10*time.Second), "secret-provider-body"))
		}
		return nil
	}, func() time.Time { return now }, func(_ context.Context, delay time.Duration) error {
		sleeps++
		if delay != 11*time.Second {
			t.Fatalf("cooldown buffer incorrect: %s", delay)
		}
		now = now.Add(delay)
		return nil
	})
	if err != nil || calls != 2 || sleeps != 1 {
		t.Fatalf("unexpected wait outcome: err=%v calls=%d sleeps=%d", err, calls, sleeps)
	}
	if strings.Contains(log.String(), "secret-provider-body") {
		t.Fatal("wait log exposed provider details")
	}
}

func TestSearchWaitStopsOnOrdinaryErrorsOrDisabledWait(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, known := range []bool{false, true} {
			if enabled && known {
				continue
			}
			calls, sleeps := 0, 0
			failure := errors.New("authentication or unknown response")
			if known {
				failure = cooldownFailure(time.Now().Add(time.Minute), "rate")
			}
			err := runSearchWithWaitClock(context.Background(), enabled, io.Discard, func() error {
				calls++
				return failure
			}, time.Now, func(context.Context, time.Duration) error { sleeps++; return nil })
			if err != failure || calls != 1 || sleeps != 0 {
				t.Fatalf("unexpected retry: err=%v calls=%d sleeps=%d", err, calls, sleeps)
			}
		}
	}
}

func TestSearchWaitCancellationPreventsResume(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := runSearchWithWaitClock(ctx, true, io.Discard, func() error {
		calls++
		return cooldownFailure(time.Now().Add(time.Hour), "rate")
	}, time.Now, func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancelled wait resumed: err=%v calls=%d", err, calls)
	}
}

func TestSearchCooldownErrorsNeverEchoReasons(t *testing.T) {
	err := cooldownFailure(time.Now(), "secret-cookie-token-response-body")
	if strings.Contains(err.Error(), "secret-cookie") {
		t.Fatal("cooldown error exposed its untrusted reason")
	}
}

func TestSearchWaitTransientBackoffCapsWithoutGivingUp(t *testing.T) {
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	var delays []time.Duration
	calls := 0
	var log bytes.Buffer
	err := runSearchWithWaitClock(context.Background(), true, &log, func() error {
		calls++
		if calls <= 8 {
			return fmt.Errorf("provider: %w", transientFailure("private-proxy-error"))
		}
		return nil
	}, func() time.Time { return now }, func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		now = now.Add(delay)
		return nil
	})
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	if err != nil || calls != 9 || len(delays) != len(want) {
		t.Fatalf("transient retry gave up or spun: err=%v calls=%d delays=%v", err, calls, delays)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("backoff %d: got %s want %s", i, delays[i], want[i])
		}
	}
	if strings.Contains(log.String(), "private-proxy-error") || strings.Contains(transientFailure("private-proxy-error").Error(), "private-proxy-error") {
		t.Fatal("transient error leaked private diagnostics")
	}
}

func TestSearchWaitNeverShortensRateCooldownForTransientError(t *testing.T) {
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	until := now.Add(10 * time.Minute)
	calls := 0
	err := runSearchWithWaitClock(context.Background(), true, io.Discard, func() error {
		calls++
		if calls == 1 {
			return errors.Join(transientFailure("network"), cooldownFailure(until, "server rate limit"))
		}
		return nil
	}, func() time.Time { return now }, func(_ context.Context, delay time.Duration) error {
		if delay != 10*time.Minute+time.Second {
			t.Fatalf("server cooldown shortened to %s", delay)
		}
		now = now.Add(delay)
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("unexpected combined cooldown result: err=%v calls=%d", err, calls)
	}
}

func TestSearchTransientWaitDisabledAndCancellation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		calls, sleeps := 0, 0
		err := runSearchWithWaitClock(ctx, enabled, io.Discard, func() error {
			calls++
			return transientFailure("network")
		}, time.Now, func(ctx context.Context, _ time.Duration) error {
			sleeps++
			cancel()
			return ctx.Err()
		})
		cancel()
		if calls != 1 || (!enabled && sleeps != 0) || (enabled && (!errors.Is(err, context.Canceled) || sleeps != 1)) {
			t.Fatalf("unexpected transient wait: enabled=%v err=%v calls=%d sleeps=%d", enabled, err, calls, sleeps)
		}
	}
}
