package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStoreRetainsCandidateHistoryWithoutDuplicatingTakenNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.jsonl")
	cfg := sourceConfig()
	s, err := openStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.appendBatch([]lookupResult{
		{Name: "aaa", Status: "taken"}, {Name: "bbb", Status: "taken"},
		{Name: "bbb", Status: "candidate"}, {Name: "ccc", Status: "candidate"},
		{Name: "bbb", Status: "taken"},
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = openStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.done) != 3 || len(s.latest) != 2 || !reflect.DeepEqual(s.order, []string{"bbb", "ccc"}) {
		t.Fatalf("unexpected retained state: done=%v latest=%v order=%v", s.done, s.latest, s.order)
	}
	if !reflect.DeepEqual(s.pendingCandidates(), []string{"ccc"}) {
		t.Fatalf("obsolete candidates replayed: %v", s.pendingCandidates())
	}
}

func BenchmarkStoreReplay100k(b *testing.B) {
	cfg := config{Version: 1, Length: 4, Alphabet: "abcdefghijklmnopqrstuvwxyz0123456789", Seed: 1}
	path := filepath.Join(b.TempDir(), "replay.jsonl")
	file, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	buffer := bufio.NewWriter(file)
	encoder := json.NewEncoder(buffer)
	if err := encoder.Encode(struct {
		Type string `json:"type"`
		config
	}{"config", persistentConfig(cfg)}); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 100000; i++ {
		status := "taken"
		if i%10 == 0 {
			status = "candidate"
		}
		if err := encoder.Encode(struct {
			Type string `json:"type"`
			lookupResult
			CheckedAt string `json:"checked_at"`
		}{"lookup", lookupResult{Name: nameAt(i, cfg), Status: status}, "2026-11-05T19:00:00Z"}); err != nil {
			b.Fatal(err)
		}
	}
	if err := buffer.Flush(); err != nil {
		b.Fatal(err)
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := openStore(path, cfg)
		if err != nil {
			b.Fatal(err)
		}
		if len(s.done) != 100000 || len(s.pendingCandidates()) != 10000 {
			b.Fatal("replay lost observations")
		}
		s.Close()
	}
}
