package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sourceConfig() config {
	return config{Version: 1, Length: 3, Alphabet: "abcdefghijklmnopqrstuvwxyz", Seed: 1}
}

func TestWordlistOrderDedupAndSourceBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.txt")
	if err := os.WriteFile(path, []byte(" DEF\nabc\n\nDEF\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := configureSource(sourceConfig(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	names, err := discoveryNames(cfg)
	if err != nil || !reflect.DeepEqual(names, []string{"def", "abc"}) {
		t.Fatalf("names=%v err=%v", names, err)
	}
	journalPath := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := openStore(journalPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = openStore(journalPath, cfg)
	if err != nil {
		t.Fatalf("runtime source path must not break metadata resume: %v", err)
	}
	s.Close()
	if err := os.WriteFile(path, []byte("abc\ndef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := discoveryNames(cfg); err == nil {
		t.Fatal("changed wordlist order was accepted")
	}
	newCfg, err := configureSource(sourceConfig(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if s, err := openStore(journalPath, newCfg); err == nil {
		s.Close()
		t.Fatal("different source metadata was accepted")
	}
}

func TestPatternAndLegacyShuffle(t *testing.T) {
	cfg := sourceConfig()
	names, err := discoveryNames(cfg)
	if err != nil {
		t.Fatal(err)
	}
	order := rand.New(rand.NewSource(cfg.Seed)).Perm(namespaceSize(cfg))
	for i, index := range order {
		if names[i] != nameAt(index, cfg) {
			t.Fatal("legacy shuffle changed")
		}
	}
	cfg, err = configureSource(cfg, "", "a??")
	if err != nil {
		t.Fatal(err)
	}
	names, err = discoveryNames(cfg)
	if err != nil || len(names) != 26*26 {
		t.Fatalf("names=%d err=%v", len(names), err)
	}
	for _, name := range names {
		if name[0] != 'a' {
			t.Fatalf("pattern mismatch: %s", name)
		}
	}
	for _, pattern := range []string{"??", "a*!", "A??"} {
		if _, err := configureSource(sourceConfig(), "", pattern); err == nil {
			t.Fatalf("invalid pattern accepted: %s", pattern)
		}
	}
}

func TestDirectPatternExpansionPreservesFilteredLegacyOrder(t *testing.T) {
	for _, tc := range []struct {
		alphabet string
		pattern  string
	}{
		{"abcdefghijklmnopqrstuvwxyz", "a??"},
		{"abcdefghijklmnopqrstuvwxyz", "?z?"},
		{"abcdefghijklmnopqrstuvwxyz", "abc"},
		{"ab01", "????"},
		{"abcdefghijklmnopqrstuvwxyz0123456789", "a?0?"},
	} {
		for _, seed := range []int64{1, 42} {
			cfg := config{Version: 1, Length: len(tc.pattern), Alphabet: tc.alphabet, Seed: seed, Pattern: tc.pattern}
			got, err := discoveryNames(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var filtered []string
			for value := 0; value < namespaceSize(cfg); value++ {
				name, matches := nameAt(value, cfg), true
				for i := range tc.pattern {
					if tc.pattern[i] != '?' && tc.pattern[i] != name[i] {
						matches = false
						break
					}
				}
				if matches {
					filtered = append(filtered, name)
				}
			}
			order := rand.New(rand.NewSource(seed)).Perm(len(filtered))
			want := make([]string, len(order))
			for i, index := range order {
				want[i] = filtered[index]
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("pattern %q alphabet=%q seed=%d changed order", tc.pattern, tc.alphabet, seed)
			}
		}
	}
}

func TestDiscoveryRefusesToAppendToInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.txt")
	original := []byte("abc\ndef\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := configureSource(sourceConfig(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := runDiscovery(context.Background(), cfg, 1, 1, path, io.Discard, io.Discard, nil); err == nil {
		t.Fatal("input/output collision accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("input changed: %q error=%v", after, err)
	}
}

func TestFreshDiscoveryRechecksSavedNamesAndPreservesHistory(t *testing.T) {
	dir := t.TempDir()
	input, journalPath := filepath.Join(dir, "names.txt"), filepath.Join(dir, "results.jsonl")
	if err := os.WriteFile(input, []byte("abc\ndef\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := configureSource(sourceConfig(), input, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := openStore(journalPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.appendBatch([]lookupResult{{Name: "abc", Status: "candidate"}, {Name: "def", Status: "taken"}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var callbacks []string
	enqueue := func(name string) error { callbacks = append(callbacks, name); return nil }
	if err := runDiscovery(context.Background(), cfg, 0, 100, journalPath, io.Discard, io.Discard, enqueue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(callbacks, []string{"abc"}) {
		t.Fatalf("normal resume pending candidates=%v", callbacks)
	}
	previous := http.DefaultTransport
	calls := 0
	http.DefaultTransport = sourceTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(req.Body)
		if !bytes.Contains(body, []byte("abc")) || !bytes.Contains(body, []byte("def")) {
			t.Fatalf("fresh request omitted saved names: %s", body)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":{"u0":{"login":"abc"},"u1":null}}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
	t.Setenv("GH_TOKEN", "fake-local-test-token")
	cfg.Recheck, callbacks = true, nil
	if err := runDiscovery(context.Background(), cfg, 0, 100, journalPath, io.Discard, io.Discard, enqueue); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !reflect.DeepEqual(callbacks, []string{"def"}) {
		t.Fatalf("fresh calls=%d callbacks=%v; stale candidate must not replay", calls, callbacks)
	}
	s, err = openStore(journalPath, cfg)
	if err != nil {
		t.Fatalf("runtime refresh flag broke metadata compatibility: %v", err)
	}
	if !reflect.DeepEqual(s.order, []string{"abc", "def"}) || len(s.done) != 2 || s.latest["abc"].Status != "taken" || s.latest["def"].Status != "candidate" {
		t.Fatalf("fresh history state: order=%v done=%v latest=%v", s.order, s.done, s.latest)
	}
	s.Close()
	data, err := os.ReadFile(journalPath)
	if err != nil || bytes.Count(data, []byte("\n")) != 5 {
		t.Fatalf("history was truncated or duplicated: %q error=%v", data, err)
	}
	if bytes.Contains(data, []byte("Recheck")) || bytes.Contains(data, []byte("recheck")) {
		t.Fatal("runtime refresh flag leaked into journal metadata")
	}
	cfg.Recheck, callbacks = false, nil
	if err := runDiscovery(context.Background(), cfg, 0, 100, journalPath, io.Discard, io.Discard, enqueue); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !reflect.DeepEqual(callbacks, []string{"def"}) {
		t.Fatalf("normal resume after refresh made requests or replayed obsolete candidates: calls=%d callbacks=%v", calls, callbacks)
	}
}

func TestBatchAppendValidatesBeforeWritingAndSavesLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.jsonl")
	s, err := openStore(path, sourceConfig())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.file.Stat()
	if err := s.appendBatch([]lookupResult{{Name: "abc", Status: "candidate"}, {Name: "def", Status: "unknown"}}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	after, _ := s.file.Stat()
	if after.Size() != before.Size() || len(s.done) != 0 {
		t.Fatal("invalid batch partially persisted")
	}
	if err := s.appendBatch([]lookupResult{{Name: "abc", Status: "candidate"}, {Name: "def", Status: "candidate"}, {Name: "abc", Status: "taken"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.pendingCandidates(); !reflect.DeepEqual(got, []string{"def"}) {
		t.Fatalf("pending=%v", got)
	}
	s.Close()
	s, err = openStore(path, sourceConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.pendingCandidates(); !reflect.DeepEqual(got, []string{"def"}) {
		t.Fatalf("resumed pending=%v", got)
	}
}

func TestBatchAppendFailedWriteNeverUpdatesDone(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "results.jsonl"), sourceConfig())
	if err != nil {
		t.Fatal(err)
	}
	s.file.Close()
	if err := s.appendBatch([]lookupResult{{Name: "abc", Status: "candidate"}}); err == nil {
		t.Fatal("closed file write succeeded")
	}
	if s.done["abc"] {
		t.Fatal("failed write marked name done")
	}
	if s.unlock != nil {
		s.unlock()
	}
}

type sourceTransport func(*http.Request) (*http.Response, error)

func (f sourceTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestPartialRetriesOnlyUnresolvedOnceThenContinues(t *testing.T) {
	bodies := []string{
		`{"data":{"u0":{"login":"abc"},"u1":null},"errors":[{"message":"temporary","path":["u1"]}]}`,
		`{"data":{"u0":null}}`,
		`{"data":{}}`,
	}
	var requests []string
	client := &githubClient{http: &http.Client{Transport: sourceTransport(func(req *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(req.Body)
		requests = append(requests, string(data))
		if len(requests) > len(bodies) {
			t.Fatal("retried more than once per unresolved alias")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(bodies[len(requests)-1]))}, nil
	})}}
	var log bytes.Buffer
	results, err := client.lookupResolved(context.Background(), []string{"abc", "def", "ghi"}, &log)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(results, []lookupResult{{Name: "abc", Status: "taken"}, {Name: "def", Status: "candidate"}}) {
		t.Fatalf("results=%v", results)
	}
	if len(requests) != 3 || strings.Contains(requests[1], "abc") || strings.Contains(requests[2], "def") {
		t.Fatalf("requests=%v", requests)
	}
	if !strings.Contains(log.String(), "ghi") || !strings.Contains(log.String(), "left unchecked") {
		t.Fatalf("log=%q", log.String())
	}
}

func TestPartialLookupErrorListsOnlyMissingAliases(t *testing.T) {
	_, err := parseResponse([]byte(`{"data":{"u0":null}}`), []string{"abc", "def"}, nil)
	var partial *partialLookupError
	if !errors.As(err, &partial) || !reflect.DeepEqual(partial.Names, []string{"def"}) {
		t.Fatalf("error=%v partial=%#v", err, partial)
	}
	_, err = parseResponse([]byte(`{"data":{"u0":{}},"errors":[{"message":"schema","path":["rateLimit"]}]}`), []string{"abc"}, nil)
	if err == nil || errors.As(err, &partial) {
		t.Fatalf("global schema problem should stop: %v", err)
	}
}
