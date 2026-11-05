package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type progressContextKey struct{}

// This replaceable snapshot is for display only. Journals remain authoritative.
type searchProgress struct {
	Platform             string             `json:"platform"`
	PID                  int                `json:"pid"`
	StartedAt            time.Time          `json:"started_at"`
	UpdatedAt            time.Time          `json:"updated_at"`
	LastProgressAt       time.Time          `json:"last_progress_at"`
	SourceTotal          int                `json:"source_total"`
	DiscoveryChecked     int                `json:"discovery_checked"`
	DiscoveryRequests    int                `json:"discovery_requests"`
	VerificationChecked  int                `json:"verification_checked"`
	VerificationRequests int                `json:"verification_requests"`
	Available            int                `json:"available"`
	PendingVerification  int                `json:"pending_verification"`
	State                string             `json:"state"`
	Rates                map[string]float64 `json:"rates"`
}

type progressTracker struct {
	mu sync.Mutex
	searchProgress
	discoveryRun, verificationRun int
}

func validateStatusPaths(path string, claim bool, protected ...string) error {
	if path == "" {
		return nil
	}
	if claim {
		return errors.New("-status-file is search-only and cannot be combined with -claim")
	}
	for _, other := range protected {
		if sameFilePath(path, other) {
			return errors.New("-status-file must differ from every input, results, and cookie file")
		}
	}
	return nil
}

func startSearchProgress(ctx context.Context, path, platform string) (context.Context, func(error), error) {
	if path == "" {
		return ctx, func(error) {}, nil
	}
	now := time.Now().UTC()
	p := &progressTracker{searchProgress: searchProgress{Platform: platform, PID: os.Getpid(), StartedAt: now, UpdatedAt: now, LastProgressAt: now, State: "running"}}
	if err := p.write(path); err != nil {
		return ctx, nil, err
	}
	ctx = context.WithValue(ctx, progressContextKey{}, p)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// Status is derived; a display-write failure must not stop search.
				_ = p.write(path)
			}
		}
	}()
	var once sync.Once
	finish := func(err error) {
		once.Do(func() {
			close(stop)
			<-done
			p.mu.Lock()
			p.State = "complete"
			if err != nil {
				p.State = "failed"
				var cooldown *searchCooldownError
				var transient *searchTransientError
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &cooldown) || errors.As(err, &transient) {
					p.State = "stopped"
				}
			}
			p.mu.Unlock()
			_ = p.write(path)
		})
	}
	return ctx, finish, nil
}

func (p *progressTracker) write(path string) error {
	p.mu.Lock()
	p.UpdatedAt = time.Now().UTC()
	seconds := max(p.UpdatedAt.Sub(p.StartedAt).Seconds(), 1e-9)
	p.Rates = map[string]float64{
		"discovery_names_per_second":       float64(p.discoveryRun) / seconds,
		"verification_names_per_second":    float64(p.verificationRun) / seconds,
		"discovery_requests_per_second":    float64(p.DiscoveryRequests) / seconds,
		"verification_requests_per_second": float64(p.VerificationRequests) / seconds,
	}
	data, err := json.MarshalIndent(p.searchProgress, "", "  ")
	p.mu.Unlock()
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".hunt-status-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func withProgress(ctx context.Context, update func(*progressTracker)) {
	p, _ := ctx.Value(progressContextKey{}).(*progressTracker)
	if p != nil {
		p.mu.Lock()
		update(p)
		p.mu.Unlock()
	}
}

func progressSource(ctx context.Context, total, checked int) {
	withProgress(ctx, func(p *progressTracker) {
		p.SourceTotal = max(p.SourceTotal, total)
		if checked > p.DiscoveryChecked {
			p.DiscoveryChecked = checked
		}
	})
}

func progressDiscovery(ctx context.Context, checked int) {
	withProgress(ctx, func(p *progressTracker) {
		if checked > p.DiscoveryChecked {
			p.discoveryRun += checked - p.DiscoveryChecked
			p.DiscoveryChecked = checked
			p.LastProgressAt = time.Now().UTC()
		}
	})
}

func progressRequest(ctx context.Context, stage string) {
	withProgress(ctx, func(p *progressTracker) {
		if stage == "discovery" {
			p.DiscoveryRequests++
		} else if stage == "verification" {
			p.VerificationRequests++
		}
	})
}

// Loaded baselines never count as work completed during this invocation. These
// counters include saved conclusive observations plus new durable observations.
func progressVerification(ctx context.Context, checked, available int) {
	withProgress(ctx, func(p *progressTracker) {
		p.VerificationChecked = max(p.VerificationChecked, checked)
		p.Available = max(p.Available, available)
	})
}

func progressVerified(ctx context.Context, result verification) {
	if result.Status != "available" && result.Status != "unavailable" {
		return
	}
	withProgress(ctx, func(p *progressTracker) {
		p.VerificationChecked++
		p.verificationRun++
		if result.Status == "available" {
			p.Available++
		}
		p.LastProgressAt = time.Now().UTC()
	})
}

func progressPending(ctx context.Context, count int) {
	withProgress(ctx, func(p *progressTracker) { p.PendingVerification = max(0, count) })
}
