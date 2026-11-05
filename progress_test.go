package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readProgress(t *testing.T, path string) searchProgress {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot searchProgress
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestProgressDisabledDoesNotChangeContext(t *testing.T) {
	ctx := context.Background()
	got, finish, err := startSearchProgress(ctx, "", "github")
	if err != nil || got != ctx {
		t.Fatalf("disabled progress changed context: %v", err)
	}
	progressSource(got, 100, 5)
	progressRequest(got, "discovery")
	progressVerified(got, verification{Status: "available"})
	finish(nil)
}

func TestProgressSnapshotCountersRetryBaselinesAndFinish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	ctx, finish, err := startSearchProgress(context.Background(), path, "github")
	if err != nil {
		t.Fatal(err)
	}
	defer finish(nil)
	if got := readProgress(t, path); got.State != "running" || got.PID != os.Getpid() {
		t.Fatalf("initial snapshot=%+v", got)
	}
	progressSource(ctx, 1000, 500)
	progressVerification(ctx, 10, 2)
	progressRequest(ctx, "discovery")
	progressDiscovery(ctx, 600)
	progressSource(ctx, 1000, 600)
	progressRequest(ctx, "verification")
	progressVerified(ctx, verification{Status: "available"})
	progressVerified(ctx, verification{Status: "unknown"})
	progressVerification(ctx, 11, 3)
	progressPending(ctx, 80)
	finish(nil)
	got := readProgress(t, path)
	if got.State != "complete" || got.SourceTotal != 1000 || got.DiscoveryChecked != 600 || got.DiscoveryRequests != 1 || got.VerificationChecked != 11 || got.VerificationRequests != 1 || got.Available != 3 || got.PendingVerification != 80 {
		t.Fatalf("final snapshot=%+v", got)
	}
	if got.LastProgressAt.Before(got.StartedAt) || got.UpdatedAt.Before(got.LastProgressAt) || got.Rates["discovery_names_per_second"] <= 0 {
		t.Fatalf("timestamps/rates invalid: %+v", got)
	}
}

func TestProgressFailureSanitizesErrorAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		err   error
		state string
	}{{errors.New("SECRET provider response"), "failed"}, {context.Canceled, "stopped"}} {
		path := filepath.Join(t.TempDir(), "status.json")
		_, finish, err := startSearchProgress(context.Background(), path, "discord")
		if err != nil {
			t.Fatal(err)
		}
		finish(tc.err)
		finish(nil)
		if readProgress(t, path).State != tc.state {
			t.Fatal("finish state was lost or finish was not idempotent")
		}
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "SECRET") {
			t.Fatal("raw failure leaked into status")
		}
	}
}

func TestProgressPathValidationProtectsSourceResultsAndCookies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "protected")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(path), "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{path, alias} {
		if validateStatusPaths(status, false, path) == nil {
			t.Fatalf("protected collision accepted: %s", status)
		}
	}
	if validateStatusPaths(path, true) == nil || validateStatusPaths("", true, path) != nil {
		t.Fatal("claim/status validation incorrect")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original" {
		t.Fatal("validation changed protected file")
	}
}

func TestProgressConcurrentUpdatesAndAtomicSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	p := &progressTracker{searchProgress: searchProgress{StartedAt: time.Now().UTC(), State: "running"}}
	ctx := context.WithValue(context.Background(), progressContextKey{}, p)
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 1000; j++ {
				progressRequest(ctx, "verification")
				progressVerified(ctx, verification{Status: "unavailable"})
			}
		}()
	}
	for i := 0; i < 30; i++ {
		if err := p.write(path); err != nil {
			t.Fatal(err)
		}
		readProgress(t, path)
	}
	workers.Wait()
	if err := p.write(path); err != nil {
		t.Fatal(err)
	}
	if got := readProgress(t, path); got.VerificationRequests != 4000 || got.VerificationChecked != 4000 {
		t.Fatalf("concurrent updates lost: %+v", got)
	}
}

func BenchmarkProgressHooks(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		name, ctx := "disabled", context.Background()
		if enabled {
			name = "enabled"
			ctx = context.WithValue(ctx, progressContextKey{}, &progressTracker{})
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				progressRequest(ctx, "verification")
			}
		})
	}
}
