package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// A known server cooldown permits an unattended search retry. This type must
// never wrap authentication, challenge, persistence, or mutation failures.
type searchCooldownError struct {
	Until  time.Time
	Reason string
}

func (e *searchCooldownError) Error() string {
	if e == nil {
		return "search rate limit cooldown"
	}
	// Do not echo arbitrary provider responses or caller-supplied reason text.
	return "search rate limit cooldown; do not retry before " + e.Until.UTC().Format(time.RFC3339Nano)
}

func cooldownFailure(until time.Time, reason string) error {
	if until.IsZero() {
		until = time.Now().Add(time.Minute)
	}
	return &searchCooldownError{Until: until, Reason: reason}
}

// Exhausted bounded connection/read/5xx attempts can be retried by search-only
// -wait mode. Deliberately omit underlying transport errors and response bodies.
type searchTransientError struct{ Reason string }

func (*searchTransientError) Error() string {
	return "temporary search connection or server failure; availability remains unknown"
}

func transientFailure(reason string) error { return &searchTransientError{Reason: reason} }

func searchChallengeResponse(body []byte) bool {
	text := strings.ToLower(string(body))
	for _, marker := range []string{"captcha", "verify you are human", "challenge-platform", "cf-chl-", "checking your browser"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func runSearchWithWait(ctx context.Context, enabled bool, log io.Writer, op func() error) error {
	return runSearchWithWaitClock(ctx, enabled, log, op, time.Now, verificationPause)
}

func runSearchWithWaitClock(ctx context.Context, enabled bool, log io.Writer, op func() error, now func() time.Time, sleep func(context.Context, time.Duration) error) error {
	if op == nil {
		return errors.New("search operation is not configured")
	}
	if log == nil {
		log = io.Discard
	}
	backoff := 15 * time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op()
		if err == nil {
			return nil
		}
		var cooldown *searchCooldownError
		var transient *searchTransientError
		hasCooldown := errors.As(err, &cooldown) && cooldown != nil
		hasTransient := errors.As(err, &transient) && transient != nil
		if !enabled || (!hasCooldown && !hasTransient) {
			return err
		}
		delay := time.Duration(0)
		if hasCooldown {
			resume := cooldown.Until.Add(time.Second)
			delay = resume.Sub(now())
			if delay < time.Second {
				delay = time.Second
			}
			fmt.Fprintf(log, "Search paused for a server cooldown; resuming after %s.\n", resume.UTC().Format(time.RFC3339Nano))
		}
		if hasTransient {
			if backoff > delay {
				delay = backoff
			}
			fmt.Fprintf(log, "Temporary search connection or server failure; retrying from saved results in %s.\n", delay)
			backoff *= 2
			if backoff > 5*time.Minute {
				backoff = 5 * time.Minute
			}
		}
		if err := sleep(ctx, delay); err != nil {
			return err
		}
		fmt.Fprintln(log, "Resuming search from saved results.")
	}
}
