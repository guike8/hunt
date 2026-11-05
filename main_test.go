package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type finderTestTransport func(*http.Request) (*http.Response, error)

func (f finderTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func finderTestResponse(status int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}

func finderTestBody(t *testing.T, owners ...any) string {
	t.Helper()
	data := map[string]any{"rateLimit": map[string]any{"remaining": 4000, "resetAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "cost": 1}}
	for i, owner := range owners {
		data["u"+string(rune('0'+i))] = owner
	}
	encoded, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func finderTestClient(transport finderTestTransport) *githubClient {
	return &githubClient{
		http:  &http.Client{Transport: transport},
		token: "test-secret-token",
		sleep: func(context.Context, time.Duration) error { return nil },
	}
}

func TestLookupUserOrganizationAndMissingOwner(t *testing.T) {
	calls := 0
	client := finderTestClient(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodPost || req.URL.String() != "https://api.github.com/graphql" {
			t.Fatalf("unexpected GraphQL request: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer test-secret-token" {
			t.Fatal("missing bearer authorization")
		}
		var payload struct{ Query string }
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if strings.Count(payload.Query, "repositoryOwner") != 3 {
			t.Fatalf("expected three owners in one query: %s", payload.Query)
		}
		return finderTestResponse(200, finderTestBody(t,
			map[string]any{"login": "abc", "__typename": "User"},
			map[string]any{"login": "def", "__typename": "Organization"}, nil), nil), nil
	})
	got, err := client.lookup(context.Background(), []string{"abc", "def", "ghi"})
	if err != nil {
		t.Fatal(err)
	}
	want := []lookupResult{{Name: "abc", Status: "taken"}, {Name: "def", Status: "taken"}, {Name: "ghi", Status: "candidate"}}
	if !reflect.DeepEqual(got, want) || calls != 1 {
		t.Fatalf("got %#v in %d requests, want %#v in one request", got, calls, want)
	}
}

func TestLookupUnknownResponsesNeverBecomeCandidates(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", 401, `{"message":"Bad credentials"}`},
		{"forbidden", 403, `{"message":"Resource not accessible by integration"}`},
		{"httpRateLimit", 429, `{"message":"API rate limit exceeded"}`},
		{"malformedJSON", 200, `<html>temporarily unavailable</html>`},
		{"missingAlias", 200, `{"data":{"rateLimit":{"remaining":4000,"resetAt":"2099-01-01T00:00:00Z"}}}`},
		{"schemaError", 200, `{"errors":[{"type":"GRAPHQL_VALIDATION_FAILED","message":"Cannot query field repositoryOwner"}]}`},
		{"graphQLRateLimit", 200, `{"data":{"u0":null,"rateLimit":{"remaining":0,"resetAt":"2099-01-01T00:00:00Z"}},"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`},
		{"aliasError", 200, `{"data":{"u0":null,"rateLimit":{"remaining":4000,"resetAt":"2099-01-01T00:00:00Z"}},"errors":[{"type":"FORBIDDEN","message":"Owner lookup denied","path":["u0"]}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client := finderTestClient(func(req *http.Request) (*http.Response, error) {
				calls++
				return finderTestResponse(tt.status, tt.body, nil), nil
			})
			got, err := client.lookup(context.Background(), []string{"abc"})
			if err == nil {
				t.Fatal("expected error for unknown owner state")
			}
			for _, result := range got {
				if result.Status == "candidate" {
					t.Fatalf("unknown response became candidate: %#v", got)
				}
			}
			if calls != 1 {
				t.Fatalf("unexpected retry of non-transient response: %d calls", calls)
			}
		})
	}
}

func TestLookupAliasErrorKeepsOnlyConclusiveAliases(t *testing.T) {
	client := finderTestClient(func(*http.Request) (*http.Response, error) {
		body := `{"data":{"u0":{"login":"abc","__typename":"User"},"u1":null,"u2":null,"rateLimit":{"remaining":4000,"resetAt":"2099-01-01T00:00:00Z"}},"errors":[{"type":"FORBIDDEN","message":"Owner lookup denied","path":["u1"]}]}`
		return finderTestResponse(200, body, nil), nil
	})
	got, err := client.lookup(context.Background(), []string{"abc", "def", "ghi"})
	if err == nil {
		t.Fatal("expected error for the inconclusive alias")
	}
	for _, result := range got {
		if result.Name == "def" {
			t.Fatalf("errored alias incorrectly classified: %#v", result)
		}
	}
}

func TestLookupTransientRetryPreservesExactQuery(t *testing.T) {
	var requests []string
	var delays []time.Duration
	client := finderTestClient(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, string(body))
		if len(requests) == 1 {
			return finderTestResponse(503, `{"message":"Service unavailable"}`, nil), nil
		}
		return finderTestResponse(200, finderTestBody(t, nil, map[string]any{"login": "def", "__typename": "User"}), nil), nil
	})
	client.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	got, err := client.lookup(context.Background(), []string{"abc", "def"})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0] != requests[1] {
		t.Fatalf("retry changed query or request count: %#v", requests)
	}
	if len(delays) == 0 {
		t.Fatal("transient retry did not back off")
	}
	if len(got) != 2 || got[0].Name != "abc" || got[1].Name != "def" {
		t.Fatalf("retry lost or reordered names: %#v", got)
	}
}

func TestLookupTransportRetryExhaustionIsBounded(t *testing.T) {
	calls := 0
	client := finderTestClient(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("temporary transport failure")
	})
	got, err := client.lookup(context.Background(), []string{"abc"})
	if err == nil || len(got) != 0 || calls < 1 || calls > 4 {
		t.Fatalf("unexpected bounded retry result: results=%#v err=%v calls=%d", got, err, calls)
	}
}

func TestLookupExhaustedBudgetStopsWithoutAnotherRequest(t *testing.T) {
	calls := 0
	client := finderTestClient(func(*http.Request) (*http.Response, error) {
		calls++
		body := `{"data":{"u0":null,"rateLimit":{"remaining":0,"resetAt":"2099-01-01T00:00:00Z"}}}`
		return finderTestResponse(200, body, http.Header{"X-Ratelimit-Remaining": []string{"0"}}), nil
	})
	got, err := client.lookup(context.Background(), []string{"abc"})
	if len(got) != 1 || got[0].Name != "abc" || got[0].Status != "candidate" {
		t.Fatalf("exhausting the budget lost the conclusive response: %#v, %v", got, err)
	}
	if err == nil {
		t.Fatal("exhausted budget did not signal a stop")
	}
	_, nextErr := client.lookup(context.Background(), []string{"def"})
	if nextErr == nil || calls != 1 {
		t.Fatalf("exhausted budget allowed another request: calls=%d err=%v", calls, nextErr)
	}
}

func TestLookupCancellationStopsTransientRetry(t *testing.T) {
	calls := 0
	client := finderTestClient(func(*http.Request) (*http.Response, error) {
		calls++
		return finderTestResponse(503, `{"message":"Service unavailable"}`, nil), nil
	})
	client.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	got, err := client.lookup(context.Background(), []string{"abc"})
	if err == nil || len(got) != 0 || calls != 1 {
		t.Fatalf("cancellation did not stop retry: results=%#v err=%v calls=%d", got, err, calls)
	}
}

func TestLookupErrorDoesNotExposeToken(t *testing.T) {
	client := finderTestClient(func(*http.Request) (*http.Response, error) {
		return finderTestResponse(401, `{"message":"Rejected token test-secret-token"}`, nil), nil
	})
	_, err := client.lookup(context.Background(), []string{"abc"})
	if err == nil {
		t.Fatal("expected authentication failure")
	}
	if strings.Contains(err.Error(), "test-secret-token") {
		t.Fatalf("error exposed a credential: %v", err)
	}
}

func TestStoreReplayDeduplicatesAndRejectsMismatchedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checks.jsonl")
	cfg := config{Version: 1, Length: 3, Alphabet: "abcdefghijklmnopqrstuvwxyz", Seed: 42}
	s, err := openStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range []lookupResult{{Name: "abc", Status: "taken"}, {Name: "def", Status: "candidate"}, {Name: "abc", Status: "taken"}} {
		if err := s.append(result); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.done) != 2 || !s.done["abc"] || !s.done["def"] {
		t.Fatalf("replay lost deduplication state: %#v", s.done)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Length = 4
	wrong, err := openStore(path, cfg)
	if err == nil {
		wrong.Close()
		t.Fatal("mismatched resume configuration was accepted")
	}
}

func TestStoreRepairsPartialTailBeforeAppending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checks.jsonl")
	cfg := config{Version: 1, Length: 3, Alphabet: "abcdefghijklmnopqrstuvwxyz", Seed: 42}
	s, err := openStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.append(lookupResult{Name: "abc", Status: "taken"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"name":"interrupted`); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	s, err = openStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !s.done["abc"] || len(s.done) != 1 {
		t.Fatalf("tail recovery lost a completed result: %#v", s.done)
	}
	if err := s.append(lookupResult{Name: "def", Status: "candidate"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = openStore(path, cfg)
	if err != nil {
		t.Fatalf("journal was not appendable after recovery: %v", err)
	}
	defer s.Close()
	if len(s.done) != 2 || !s.done["abc"] || !s.done["def"] {
		t.Fatalf("recovered journal lost results: %#v", s.done)
	}
}

func TestGeneratedThreeLetterNamespaceIsCompleteAndUnique(t *testing.T) {
	cfg := config{Version: 1, Length: 3, Alphabet: "abcdefghijklmnopqrstuvwxyz", Seed: 42}
	if got := namespaceSize(cfg); got != 17576 {
		t.Fatalf("unexpected three-letter namespace size: %d", got)
	}
	seen := make(map[string]bool, namespaceSize(cfg))
	for i := 0; i < namespaceSize(cfg); i++ {
		name := nameAt(i, cfg)
		if seen[name] || !validResult(lookupResult{Name: name, Status: "taken"}, cfg) {
			t.Fatalf("duplicate or invalid generated name %q at %d", name, i)
		}
		seen[name] = true
	}
	if len(seen) != 17576 {
		t.Fatalf("generation omitted names: %d generated", len(seen))
	}
}

func TestGeneratedNamespaceBoundaries(t *testing.T) {
	tests := []struct {
		length      int
		alphabet    string
		size        int
		first, last string
	}{
		{3, "abcdefghijklmnopqrstuvwxyz", 17576, "aaa", "zzz"},
		{4, "abcdefghijklmnopqrstuvwxyz", 456976, "aaaa", "zzzz"},
		{3, "abcdefghijklmnopqrstuvwxyz0123456789", 46656, "aaa", "999"},
		{4, "abcdefghijklmnopqrstuvwxyz0123456789", 1679616, "aaaa", "9999"},
	}
	for _, tt := range tests {
		cfg := config{Version: 1, Length: tt.length, Alphabet: tt.alphabet, Seed: 42}
		if namespaceSize(cfg) != tt.size || nameAt(0, cfg) != tt.first || nameAt(tt.size-1, cfg) != tt.last {
			t.Fatalf("incorrect namespace boundaries for length=%d alphabet=%s", tt.length, tt.alphabet)
		}
	}
}

func TestStoreRefusesInvalidOrInconclusiveResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checks.jsonl")
	cfg := config{Version: 1, Length: 3, Alphabet: "abcdefghijklmnopqrstuvwxyz", Seed: 42}
	s, err := openStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, result := range []lookupResult{
		{Name: "abc", Status: "unknown"},
		{Name: "abc", Status: "available"},
		{Name: "ab1", Status: "candidate"},
		{Name: "abcd", Status: "taken"},
	} {
		if err := s.append(result); err == nil {
			t.Fatalf("invalid or inconclusive result was persisted: %#v", result)
		}
	}
	if len(s.done) != 0 {
		t.Fatalf("invalid records poisoned completion state: %#v", s.done)
	}
}

func TestStoreRejectsCorruptionBeforeFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checks.jsonl")
	cfg := config{Version: 1, Length: 3, Alphabet: "abcdefghijklmnopqrstuvwxyz", Seed: 42}
	s, err := openStore(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json}\n{\"type\":\"lookup\"}\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	s, err = openStore(path, cfg)
	if err == nil {
		s.Close()
		t.Fatal("journal corruption before final line was silently discarded")
	}
}
