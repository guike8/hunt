package main

import (
	"context"
	"errors"
	"sync"
)

// The queue has a finite logical limit, but allocates only for its actual
// backlog. A small wordlist never reserves a full namespace of string slots.
type candidateQueue struct {
	mu          sync.Mutex
	buffer      []string
	head, count int
	limit       int
	closed      bool
	readable    chan struct{}
	writable    chan struct{}
}

func newCandidateQueue(limit int) (*candidateQueue, error) {
	if limit < 1 {
		return nil, errors.New("verification queue must have positive capacity")
	}
	initial := min(limit, 32)
	return &candidateQueue{buffer: make([]string, initial), limit: limit, readable: make(chan struct{}, 1), writable: make(chan struct{}, 1)}, nil
}

func queueSignal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (q *candidateQueue) put(ctx context.Context, name string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return errors.New("verification queue is closed")
		}
		if q.count < q.limit {
			if q.count == len(q.buffer) {
				grown := make([]string, min(q.limit, len(q.buffer)*2))
				for i := 0; i < q.count; i++ {
					grown[i] = q.buffer[(q.head+i)%len(q.buffer)]
				}
				q.buffer, q.head = grown, 0
			}
			q.buffer[(q.head+q.count)%len(q.buffer)] = name
			q.count++
			q.mu.Unlock()
			queueSignal(q.readable)
			return nil
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-q.writable:
		}
	}
}

func (q *candidateQueue) take(ctx context.Context) (string, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		q.mu.Lock()
		if q.count > 0 {
			name := q.buffer[q.head]
			q.buffer[q.head] = ""
			q.head = (q.head + 1) % len(q.buffer)
			q.count--
			q.mu.Unlock()
			queueSignal(q.writable)
			return name, true, nil
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return "", false, nil
		}
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-q.readable:
		}
	}
}

func (q *candidateQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	queueSignal(q.readable)
	queueSignal(q.writable)
}

func (q *candidateQueue) size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.count
}
