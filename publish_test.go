package main

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type publishTestWriter struct {
	bytes.Buffer
	short bool
	syncs int
	err   error
}

func (w *publishTestWriter) WriteString(s string) (int, error) {
	if w.short {
		return len(s) - 1, nil
	}
	return w.Buffer.WriteString(s)
}

func (w *publishTestWriter) Sync() error {
	w.syncs++
	return w.err
}

func TestPublishRequiresCompleteDurableOutput(t *testing.T) {
	writer := &publishTestWriter{}
	if err := publishResult(writer, "abc"); err != nil || writer.String() != "abc\n" || writer.syncs != 1 {
		t.Fatalf("output was not synced: %q, %d, %v", writer.String(), writer.syncs, err)
	}
	short := &publishTestWriter{short: true}
	if err := publishResult(short, "abc"); !errors.Is(err, io.ErrShortWrite) || short.syncs != 0 {
		t.Fatalf("short write acknowledged: %v, syncs=%d", err, short.syncs)
	}
	failed := &publishTestWriter{err: errors.New("storage failure")}
	if err := publishResult(failed, "abc"); err == nil {
		t.Fatal("sync failure ignored")
	}
}
