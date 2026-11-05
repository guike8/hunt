package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCandidateQueueGrowsWithBacklogAndPreservesOrder(t *testing.T) {
	q, err := newCandidateQueue(1_679_616)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.buffer) != 32 {
		t.Fatalf("unused namespace preallocated: %d slots", len(q.buffer))
	}
	for i := 0; i < 100; i++ {
		if err := q.put(context.Background(), fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(q.buffer) != 128 {
		t.Fatalf("unexpected backlog allocation: %d slots", len(q.buffer))
	}
	for i := 0; i < 70; i++ {
		name, ok, err := q.take(context.Background())
		if err != nil || !ok || name != fmt.Sprint(i) {
			t.Fatalf("FIFO lost %d: %q, %v, %v", i, name, ok, err)
		}
	}
	for i := 100; i < 250; i++ {
		q.put(context.Background(), fmt.Sprint(i))
	}
	q.close()
	for i := 70; i < 250; i++ {
		name, ok, err := q.take(context.Background())
		if err != nil || !ok || name != fmt.Sprint(i) {
			t.Fatalf("wrapped FIFO lost %d: %q, %v, %v", i, name, ok, err)
		}
	}
	if _, ok, err := q.take(context.Background()); ok || err != nil {
		t.Fatal("closed queue did not drain")
	}
}

func TestCandidateQueueCancellationWhileFull(t *testing.T) {
	q, _ := newCandidateQueue(1)
	q.put(context.Background(), "abc")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.put(ctx, "def") }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("full queue ignored cancellation")
	}
}

func BenchmarkSparseVerificationQueue(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		q, _ := newCandidateQueue(1_679_616)
		q.put(context.Background(), "abc")
		q.take(context.Background())
	}
}
