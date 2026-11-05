package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type verificationTransport func(*http.Request) (*http.Response, error)

func (f verificationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWebVerificationClassifiesOnlyExplicitName(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		code             int
	}{
		{"available", "abc is available.", "available", 200},
		{"unavailable despite suggestions", `<div>Username abc is not available.</div><span>abc123 is available.</span>`, "unavailable", 422},
		{"wrong available name", "xyz is available.", "", 200},
		{"generic not found", "<html>Not Found</html>", "", 404},
		{"challenge", "Verify you are human", "", 200},
		{"malformed unavailable", "abc is unavailable", "", 422},
		{"throttled", "abc is available.", "", 429},
		{"forbidden", "abc is available.", "", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := webVerifier{client: &http.Client{Transport: verificationTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method != "GET" || req.URL.Query().Get("value") != "abc" || req.Header.Get("Accept") != "text/fragment+html" || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
					t.Fatal("wrong validation request or credentials sent")
				}
				return &http.Response{StatusCode: tc.code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}}
			got, err := v.check(context.Background(), "abc")
			if got != tc.want || (err == nil) != (tc.want != "") {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestWebVerificationPacesAndRejectsInjection(t *testing.T) {
	requests, waits := 0, 0
	v := webVerifier{
		client: &http.Client{Transport: verificationTransport(func(req *http.Request) (*http.Response, error) {
			requests++
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("abc is available."))}, nil
		})},
		interval: 7 * time.Second,
		sleep: func(ctx context.Context, delay time.Duration) error {
			waits++
			if delay <= 0 || delay > 7*time.Second {
				t.Fatalf("invalid pause: %s", delay)
			}
			return nil
		},
	}
	for i := 0; i < 2; i++ {
		if _, err := v.check(context.Background(), "abc"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v.check(context.Background(), `abc&value=other`); err == nil {
		t.Fatal("invalid input accepted")
	}
	if requests != 2 || waits != 1 {
		t.Fatalf("got %d requests and %d pauses", requests, waits)
	}
}

func TestVerificationRejectsOutputInputCollision(t *testing.T) {
	file := filepath.Join(t.TempDir(), "candidates.txt")
	if err := os.WriteFile(file, []byte("abc\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyFile(context.Background(), file, file, io.Discard); err == nil {
		t.Fatal("input and output collision accepted")
	}
	got, _ := os.ReadFile(file)
	if string(got) != "abc\n" {
		t.Fatal("input modified")
	}
}

func TestCandidateInputAcceptsDurableJournal(t *testing.T) {
	input := []byte("{\"type\":\"config\",\"length\":3}\n{\"type\":\"lookup\",\"name\":\"abc\",\"status\":\"taken\"}\n{\"type\":\"lookup\",\"name\":\"xyz\",\"status\":\"candidate\"}\nxyz\n")
	names, err := candidateNames(input)
	if err != nil || len(names) != 1 || names[0] != "xyz" {
		t.Fatalf("unexpected candidates: %v, %v", names, err)
	}
	for _, invalid := range []string{"abc&value=xyz", "{\"type\":\"lookup\",\"name\":\"xyz\",\"status\":\"unknown\"}", "{torn"} {
		if _, err := candidateNames([]byte(invalid)); err == nil {
			t.Fatalf("invalid candidate accepted: %q", invalid)
		}
	}
}

func TestCandidateInputUsesLatestObservationAndFirstSeenOrder(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"config","length":3}`,
		`{"type":"lookup","name":"abc","status":"taken"}`,
		`{"type":"lookup","name":"def","status":"candidate"}`,
		`{"type":"lookup","name":"ghi","status":"candidate"}`,
		`{"type":"lookup","name":"abc","status":"candidate"}`,
		`{"type":"lookup","name":"def","status":"taken"}`,
		`{"type":"lookup","name":"ghi","status":"candidate"}`,
		"jkl",
		`{"type":"lookup","name":"jkl","status":"taken"}`,
		`{"type":"lookup","name":"mno","status":"taken"}`,
		"mno",
		"mno",
	}, "\n")
	got, err := candidateNames([]byte(input))
	want := []string{"abc", "ghi", "mno"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("latest observations or order lost: got=%v err=%v want=%v", got, err, want)
	}
	for _, invalid := range []string{
		`{"type":"lookup","name":"bad&name","status":"taken"}`,
		`{"type":"lookup","name":"abc","status":"unknown"}`,
	} {
		if _, err := candidateNames([]byte(invalid)); err == nil {
			t.Fatalf("invalid observation accepted: %s", invalid)
		}
	}
}

func verificationTestHTTP(t *testing.T, s *verificationSession, now *time.Time, calls *int, code int) {
	t.Helper()
	s.verifier.now = func() time.Time { return *now }
	s.verifier.sleep = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		*now = now.Add(delay)
		return nil
	}
	s.verifier.client = &http.Client{Transport: verificationTransport(func(req *http.Request) (*http.Response, error) {
		*calls++
		name := req.URL.Query().Get("value")
		body := name + " is available."
		if code == http.StatusUnprocessableEntity {
			body = "Username " + name + " is not available."
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
}

func TestVerificationSessionCachesAndRechecksStaleResults(t *testing.T) {
	for _, tc := range []struct {
		status string
		code   int
		ttl    time.Duration
	}{
		{"available", http.StatusOK, 30 * time.Second},
		{"unavailable", http.StatusUnprocessableEntity, 24 * time.Hour},
	} {
		t.Run(tc.status, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "verified.jsonl")
			now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
			calls := 0
			s, err := openVerificationSession(path)
			if err != nil {
				t.Fatal(err)
			}
			verificationTestHTTP(t, s, &now, &calls, tc.code)
			first, err := s.verify(context.Background(), "abc")
			if err != nil || first.Status != tc.status || calls != 1 {
				t.Fatalf("unexpected initial verification: %#v err=%v calls=%d", first, err, calls)
			}
			now = now.Add(tc.ttl - time.Second)
			cached, err := s.verify(context.Background(), "abc")
			if err != nil || calls != 1 || !cached.CheckedAt.Equal(first.CheckedAt) {
				t.Fatalf("fresh result was not cached: %#v err=%v calls=%d", cached, err, calls)
			}
			now = now.Add(2 * time.Second)
			refreshed, err := s.verify(context.Background(), "abc")
			if err != nil || calls != 2 || !refreshed.CheckedAt.After(first.CheckedAt) {
				t.Fatalf("stale result was not refreshed: %#v err=%v calls=%d", refreshed, err, calls)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = openVerificationSession(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			verificationTestHTTP(t, s, &now, &calls, tc.code)
			if _, err := s.verify(context.Background(), "abc"); err != nil || calls != 2 {
				t.Fatalf("cache did not survive restart: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestVerificationForcedRefreshPreservesPacingAndCooldown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verified.jsonl")
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	start := now
	calls := 0
	s, err := openVerificationSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	verificationTestHTTP(t, s, &now, &calls, http.StatusOK)
	if _, err := s.verify(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	s.force = true
	if result, err := s.verify(context.Background(), "abc"); err != nil || result.Status != "available" || calls != 2 {
		t.Fatalf("forced refresh reused a fresh cache: %#v err=%v calls=%d", result, err, calls)
	}
	if now.Sub(start) < 7*time.Second {
		t.Fatalf("forced refresh bypassed request spacing: elapsed=%s", now.Sub(start))
	}
	s.verifier.cooldown = now.Add(time.Minute)
	if result, err := s.verify(context.Background(), "abc"); err == nil || result.Status != "" || calls != 2 {
		t.Fatalf("forced refresh bypassed cooldown: %#v err=%v calls=%d", result, err, calls)
	}
}

func TestVerificationSessionPersistsRateCooldown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verified.jsonl")
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	calls := 0
	s, err := openVerificationSession(path)
	if err != nil {
		t.Fatal(err)
	}
	verificationTestHTTP(t, s, &now, &calls, http.StatusOK)
	s.verifier.client = &http.Client{Transport: verificationTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"120"}}, Body: io.NopCloser(strings.NewReader("abc is available."))}, nil
	})}
	if result, err := s.verify(context.Background(), "abc"); err == nil || result.Status != "" || calls != 1 {
		t.Fatalf("rate response was retried or classified: %#v err=%v calls=%d", result, err, calls)
	}
	if _, err := s.verify(context.Background(), "abc"); err == nil || calls != 1 {
		t.Fatalf("same session ignored cooldown: err=%v calls=%d", err, calls)
	}
	s.Close()
	now = now.Add(20 * time.Second)
	s, err = openVerificationSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	verificationTestHTTP(t, s, &now, &calls, http.StatusOK)
	if _, err := s.verify(context.Background(), "abc"); err == nil || calls != 1 {
		t.Fatalf("restart ignored cooldown: err=%v calls=%d", err, calls)
	}
	now = now.Add(101 * time.Second)
	if result, err := s.verify(context.Background(), "abc"); err != nil || result.Status != "available" || calls != 2 {
		t.Fatalf("expired cooldown did not permit a new check: %#v err=%v calls=%d", result, err, calls)
	}
}

func TestVerificationSessionPreservesCadenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verified.jsonl")
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	start := now
	calls := 0
	s, err := openVerificationSession(path)
	if err != nil {
		t.Fatal(err)
	}
	verificationTestHTTP(t, s, &now, &calls, http.StatusUnprocessableEntity)
	if _, err := s.verify(context.Background(), "abc"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	now = now.Add(time.Second)
	s, err = openVerificationSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	verificationTestHTTP(t, s, &now, &calls, http.StatusUnprocessableEntity)
	if _, err := s.verify(context.Background(), "def"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || now.Sub(start) < 7*time.Second {
		t.Fatalf("restart bypassed pacing: calls=%d elapsed=%s", calls, now.Sub(start))
	}
}

func TestVerificationRetriesRespectCadenceAndCancellation(t *testing.T) {
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	var starts []time.Time
	v := webVerifier{interval: 7 * time.Second, now: func() time.Time { return now }}
	v.sleep = func(_ context.Context, delay time.Duration) error { now = now.Add(delay); return nil }
	v.client = &http.Client{Transport: verificationTransport(func(req *http.Request) (*http.Response, error) {
		starts = append(starts, now)
		if len(starts) == 1 {
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
		}
		if len(starts) == 2 {
			return nil, errors.New("temporary connection error")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("abc is available."))}, nil
	})}
	if status, err := v.check(context.Background(), "abc"); err != nil || status != "available" || len(starts) != 3 {
		t.Fatalf("unexpected retry outcome: status=%s err=%v attempts=%d", status, err, len(starts))
	}
	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < 7*time.Second {
			t.Fatal("retry bypassed cadence")
		}
	}
	v.sleep = func(context.Context, time.Duration) error { return context.Canceled }
	if _, err := v.check(context.Background(), "abc"); !errors.Is(err, context.Canceled) || len(starts) != 3 {
		t.Fatalf("cancellation did not prevent another request: err=%v attempts=%d", err, len(starts))
	}
}

func TestVerificationSessionRepairsPartialTailAndLocksJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verified.jsonl")
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	saved := verification{Name: "abc", Status: "unavailable", CheckedAt: now, Source: validationURL}
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\n{\"name\":\"torn")...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openVerificationSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := openVerificationSession(path); err == nil {
		other.Close()
		s.Close()
		t.Fatal("same journal accepted a second writer")
	}
	calls := 0
	verificationTestHTTP(t, s, &now, &calls, http.StatusUnprocessableEntity)
	if result, err := s.verify(context.Background(), "abc"); err != nil || result.Status != "unavailable" || calls != 0 {
		t.Fatalf("recovery lost completed record: %#v err=%v calls=%d", result, err, calls)
	}
	if _, err := s.verify(context.Background(), "def"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = openVerificationSession(path)
	if err != nil {
		t.Fatalf("recovered file could not be reopened: %v", err)
	}
	defer s.Close()
	if len(s.cache) != 2 {
		t.Fatalf("recovered journal lost results: %#v", s.cache)
	}
}

func TestVerificationCooldownUsesConservativeMinimum(t *testing.T) {
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	for value, duration := range map[string]time.Duration{
		"":        time.Minute,
		"garbage": time.Minute,
		"0":       time.Minute,
		"120":     2 * time.Minute,
		now.Add(3 * time.Minute).Format(http.TimeFormat): 3 * time.Minute,
	} {
		if got := verificationCooldown(value, now); !got.Equal(now.Add(duration)) {
			t.Fatalf("%s: got %s, want %s", fmt.Sprintf("Retry-After %q", value), got, now.Add(duration))
		}
	}
}
