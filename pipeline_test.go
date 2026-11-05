package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStreamFindStopsOnVerifiedAvailability(t *testing.T) {
	var output bytes.Buffer
	var produced atomic.Int32
	checked := 0
	err := streamFind(context.Background(), 1, &output, io.Discard,
		func(ctx context.Context, enqueue func(string) error, log io.Writer) error {
			for i := 0; i < 10000; i++ {
				name := "abc"
				if i == 1 {
					name = "def"
				}
				if err := enqueue(name); err != nil {
					return err
				}
				produced.Add(1)
			}
			return nil
		}, func(ctx context.Context, name string) (verification, error) {
			checked++
			status := "unavailable"
			if name == "def" {
				status = "available"
			}
			return verification{Name: name, Status: status}, nil
		})
	if err != nil || checked != 2 || output.String() != "def\n" {
		t.Fatalf("unexpected first-find result: %q, checks=%d, err=%v", output.String(), checked, err)
	}
	if produced.Load() >= 10000 {
		t.Fatal("early stop did not cancel discovery")
	}
}

func TestStreamFindPropagatesUnknownAndCancelsProducer(t *testing.T) {
	var output bytes.Buffer
	var produced atomic.Int32
	err := streamFind(context.Background(), 1, &output, io.Discard,
		func(ctx context.Context, enqueue func(string) error, log io.Writer) error {
			for {
				if err := enqueue("abc"); err != nil {
					return err
				}
				produced.Add(1)
			}
		}, func(context.Context, string) (verification, error) {
			return verification{}, errors.New("rate limited")
		})
	if err == nil || !strings.Contains(err.Error(), "rate limited") || output.Len() != 0 {
		t.Fatalf("unknown verification produced a result: %q %v", output.String(), err)
	}
	if produced.Load() > 34 {
		t.Fatalf("verification queue did not apply backpressure: %d", produced.Load())
	}
}

func TestStreamFindReportsDiscoveryFailure(t *testing.T) {
	err := streamFind(context.Background(), 1, io.Discard, io.Discard,
		func(context.Context, func(string) error, io.Writer) error { return errors.New("authentication failed") },
		func(context.Context, string) (verification, error) {
			t.Fatal("verifier called without a candidate")
			return verification{}, nil
		})
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("lost discovery error: %v", err)
	}
}

func TestStreamFindSatisfiedGoalSurvivesDiscoveryStop(t *testing.T) {
	var output bytes.Buffer
	err := streamFind(context.Background(), 1, &output, io.Discard,
		func(ctx context.Context, enqueue func(string) error, log io.Writer) error {
			if err := enqueue("abc"); err != nil {
				return err
			}
			return errors.New("discovery rate limit reached")
		}, func(context.Context, string) (verification, error) {
			return verification{Name: "abc", Status: "available"}, nil
		})
	if err != nil || output.String() != "abc\n" {
		t.Fatalf("satisfied find was lost: %q, %v", output.String(), err)
	}
}

func TestStreamClaimOnlyFirstVerifiedAvailableNameAndCancels(t *testing.T) {
	var output bytes.Buffer
	var produced atomic.Int32
	checked, claims := 0, 0
	err := streamFindWithClaim(context.Background(), 1, &output, io.Discard,
		func(ctx context.Context, enqueue func(string) error, log io.Writer) error {
			for i := 0; i < 10000; i++ {
				name := "def"
				if i == 0 {
					name = "abc"
				}
				if err := enqueue(name); err != nil {
					return err
				}
				produced.Add(1)
			}
			return nil
		}, func(ctx context.Context, name string) (verification, error) {
			checked++
			status := "available"
			if name == "abc" {
				status = "unavailable"
			}
			return verification{Name: name, Status: status}, nil
		}, func(ctx context.Context, name string) error {
			claims++
			if name != "def" || output.Len() != 0 || ctx.Err() != nil {
				t.Fatalf("claim was premature, mismatched, or producer cancellation leaked into claim context: name=%s output=%q error=%v", name, output.String(), ctx.Err())
			}
			return nil
		})
	if err != nil || claims != 1 || checked != 2 || output.String() != "def\n" || produced.Load() >= 10000 {
		t.Fatalf("claim pipeline: error=%v claims=%d checks=%d output=%q produced=%d", err, claims, checked, output.String(), produced.Load())
	}
}

func TestStreamClaimFailureNeverTriesAnotherNameOrPrintsSuccess(t *testing.T) {
	var output bytes.Buffer
	claims, checks := 0, 0
	err := streamFindWithClaim(context.Background(), 1, &output, io.Discard,
		func(ctx context.Context, enqueue func(string) error, log io.Writer) error {
			for _, name := range []string{"abc", "def", "ghi"} {
				if err := enqueue(name); err != nil {
					return err
				}
			}
			return nil
		}, func(ctx context.Context, name string) (verification, error) {
			checks++
			return verification{Name: name, Status: "available"}, nil
		}, func(ctx context.Context, name string) error {
			claims++
			return errors.New("rename result is ambiguous")
		})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || claims != 1 || checks != 1 || output.Len() != 0 {
		t.Fatalf("claim retried or reported success: error=%v claims=%d checks=%d output=%q", err, claims, checks, output.String())
	}
}

func TestStreamClaimNeverRunsWithoutMatchingAvailability(t *testing.T) {
	for _, tc := range []struct {
		name, resultName, status string
		verifyError              error
		wantError                bool
	}{
		{"abc", "abc", "unavailable", nil, false},
		{"abc", "def", "available", nil, true},
		{"abc", "abc", "unknown", nil, true},
		{"abcde", "abcde", "available", nil, true},
		{"abc", "abc", "available", errors.New("verification failed"), true},
	} {
		var output bytes.Buffer
		claims := 0
		err := streamFindWithClaim(context.Background(), 1, &output, io.Discard,
			func(ctx context.Context, enqueue func(string) error, log io.Writer) error { return enqueue(tc.name) },
			func(ctx context.Context, name string) (verification, error) {
				return verification{Name: tc.resultName, Status: tc.status}, tc.verifyError
			}, func(ctx context.Context, name string) error { claims++; return nil })
		if (err != nil) != tc.wantError || claims != 0 || output.Len() != 0 {
			t.Fatalf("unsafe claim for %+v: error=%v claims=%d output=%q", tc, err, claims, output.String())
		}
	}
}

func TestClaimFlagsRequireExplicitCompleteGitHubOptIn(t *testing.T) {
	for _, tc := range []struct {
		platform         string
		enabled          bool
		cookies, account string
		find             int
		explicit         map[string]bool
		wantError        bool
	}{
		{"github", false, "", "", 0, nil, false},
		{"github", false, "secret-file", "", 0, nil, true},
		{"github", false, "", "", 0, map[string]bool{"claim-account": true}, true},
		{"github", true, "", "expected", 1, nil, true},
		{"github", true, "secret-file", "", 1, nil, true},
		{"discord", true, "secret-file", "expected", 1, nil, true},
		{"github", true, "secret-file", "expected", 2, nil, true},
		{"github", true, "secret-file", "expected", 1, map[string]bool{"verify": true}, true},
		{"github", true, "secret-file", "expected", 1, map[string]bool{"live-verify": true}, true},
		{"github", true, "secret-file", "expected", 0, nil, false},
		{"github", true, "secret-file", "expected", 1, nil, false},
	} {
		find, err := validateClaimFlags(tc.platform, tc.enabled, tc.cookies, tc.account, tc.find, tc.explicit)
		if (err != nil) != tc.wantError || (!tc.wantError && tc.enabled && find != 1) {
			t.Fatalf("claim flags validation: error=%v find=%d enabled=%v platform=%s", err, find, tc.enabled, tc.platform)
		}
		if err != nil && strings.Contains(err.Error(), "secret-file") {
			t.Fatal("claim validation leaked cookie path")
		}
	}
	for _, name := range []string{"claim", "claim-cookies", "claim-account"} {
		if err := validatePlatformFlags("discord", map[string]bool{name: true}); err == nil {
			t.Fatalf("Discord accepted -%s", name)
		}
	}
}

func TestClaimCookieFileCannotBeUsedAsJournalOrWordlist(t *testing.T) {
	dir := t.TempDir()
	cookies := filepath.Join(dir, "private-cookie-file")
	if err := os.WriteFile(cookies, []byte("private fixture contents"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{
		{cookies, "other-output"},
		{"other-output", cookies},
		{filepath.Join(dir, ".", "private-cookie-file")},
	} {
		if err := validateClaimPaths(cookies, paths...); err == nil || strings.Contains(err.Error(), cookies) {
			t.Fatalf("cookie collision accepted or secret path leaked: %v", err)
		}
	}
	if err := validateClaimPaths(cookies, filepath.Join(dir, "results.jsonl"), ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cookies)
	if err != nil || string(data) != "private fixture contents" {
		t.Fatal("claim path validation modified the cookie file")
	}
}
