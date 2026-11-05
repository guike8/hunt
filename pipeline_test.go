package main

import (
	"bytes"
	"context"
	"errors"
	"io"
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
