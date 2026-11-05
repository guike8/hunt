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

type verificationReadFailure struct{}

func (verificationReadFailure) Read([]byte) (int, error) {
	return 0, errors.New("private read failure")
}
func (verificationReadFailure) Close() error { return nil }

func TestWebVerificationClassifiesOnlyExplicitName(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		code             int
	}{
		{"available", "abc is available.", "available", 200},
		{"unavailable despite suggestions", `<div>Username abc is not available.</div><span>abc123 is available.</span>`, "unavailable", 422},
		{"quoted unavailable", `<div>Username &#39;abc&#39; is unavailable.</div>`, "unavailable", 422},
		{"double quoted not available", `Username "abc" is not available.`, "unavailable", 422},
		{"unquoted unavailable", `Username abc is unavailable.`, "unavailable", 422},
		{"wrong quoted unavailable", `Username 'xyz' is unavailable.`, "", 422},
		{"suggestion only", `Suggestion: Username 'abc' is unavailable.`, "", 422},
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

func TestVerificationOnlyDefiniteRateLimitsPermitWaiting(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
		v := webVerifier{client: &http.Client{Transport: verificationTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("blocked"))}, nil
		})}}
		_, err := v.check(context.Background(), "abc")
		var cooldown *searchCooldownError
		if errors.As(err, &cooldown) != (code == http.StatusTooManyRequests) {
			t.Fatalf("HTTP%d incorrectly classified for automatic wait: %v", code, err)
		}
	}
}

func TestVerificationExhaustedTransientFailuresAreTyped(t *testing.T) {
	for _, mode := range []string{"connection", "server", "read", "challenge", "serverChallenge", "malformed", "auth"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			v := webVerifier{sleep: func(context.Context, time.Duration) error { return nil }, client: &http.Client{Transport: verificationTransport(func(*http.Request) (*http.Response, error) {
				calls++
				if mode == "connection" {
					return nil, errors.New("private proxy diagnostic")
				}
				code, body := 200, "not recognized"
				if mode == "server" {
					code = 503
				}
				if mode == "auth" {
					code = 401
				}
				if mode == "challenge" {
					body = "Verify you are human"
				}
				if mode == "serverChallenge" {
					code, body = 503, "Verify you are human"
				}
				response := &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
				if mode == "read" {
					response.Body = verificationReadFailure{}
				}
				return response, nil
			})}}
			status, err := v.check(context.Background(), "abc")
			var transient *searchTransientError
			want := mode == "connection" || mode == "server" || mode == "read"
			if status != "" || err == nil || errors.As(err, &transient) != want || (want && calls != 3) || (!want && calls != 1) {
				t.Fatalf("wrong transient classification: mode=%s status=%s err=%v calls=%d", mode, status, err, calls)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("private diagnostics leaked")
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

func TestVerificationPublicationSurvivesRestartAndUsesObservationGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verified.jsonl")
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	s, err := openVerificationSession(path)
	if err != nil {
		t.Fatal(err)
	}
	// A legacy journal has no publication acknowledgment or generation.
	if err := s.write(verification{Name: "abc", Status: "available", CheckedAt: now, Source: validationURL}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = openVerificationSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.needsPublish("abc") {
		t.Fatal("a result persisted before stdout was silently treated as delivered")
	}
	first, _ := s.recorded("abc")
	if first.Generation != 1 || s.markPublished(first) != nil || s.needsPublish("abc") {
		t.Fatalf("legacy observation could not be acknowledged: %#v", first)
	}
	// Keep the same timestamp for a second observation. Its acknowledgment
	// must be separate even when the wall clock does not advance.
	calls := 0
	verificationTestHTTP(t, s, &now, &calls, http.StatusOK)
	s.verifier.interval = 0
	s.force = true
	second, err := s.verify(context.Background(), "abc")
	if err != nil || second.Generation != 2 || !second.CheckedAt.Equal(first.CheckedAt) || !s.needsPublish("abc") {
		t.Fatalf("new observation identity was lost: %#v err=%v", second, err)
	}
	if err := s.markPublished(first); err == nil {
		t.Fatal("an old generation acknowledged a newer observation")
	}
	if err := s.markPublished(second); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = openVerificationSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.needsPublish("abc") {
		t.Fatal("durable publication acknowledgment was lost on restart")
	}
}

func TestVerificationJournalLastCompletedRowWinsAfterClockRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verified.jsonl")
	s, err := openVerificationSession(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	for i, status := range []string{"available", "unavailable"} {
		if err := s.write(verification{Name: "abc", Status: status, CheckedAt: now.Add(-time.Duration(i) * time.Hour), Source: validationURL}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = openVerificationSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	latest, found := s.recorded("abc")
	if !found || latest.Status != "unavailable" || latest.Generation != 2 || s.needsPublish("abc") {
		t.Fatalf("clock rollback restored an obsolete positive: %#v", latest)
	}
}

func TestVerificationRejectsInvalidPublicationAcknowledgments(t *testing.T) {
	for _, generation := range []uint64{0, 2} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "verified.jsonl")
			s, err := openVerificationSession(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.write(verification{Name: "abc", Status: "available", CheckedAt: time.Now().UTC(), Source: validationURL}); err != nil {
				t.Fatal(err)
			}
			if err := s.write(publicationAck{Type: "published", Name: "abc", Generation: generation}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			if reopened, err := openVerificationSession(path); err == nil {
				reopened.Close()
				t.Fatal("invalid or future acknowledgment generation was accepted")
			}
		})
	}
}

func TestProviderCooldownSurvivesResultJournalFailure(t *testing.T) {
	for _, provider := range []string{"github-signup", "discord-eligibility"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("HUNT_STATE_DIR", t.TempDir())
			gate, err := newProviderGate(provider, 0)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
			gate.now = func() time.Time { return now }
			journalFailure := func(time.Time) error { return errors.New("result journal unavailable") }
			if provider == "github-signup" {
				v := webVerifier{gate: gate, now: func() time.Time { return now }, onCooldown: journalFailure,
					client: &http.Client{Transport: verificationTransport(func(*http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"120"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
					})}}
				if _, err := v.check(context.Background(), "abc"); err == nil {
					t.Fatal("journal failure was ignored")
				}
			} else {
				d := discordClient{gate: gate, now: func() time.Time { return now }, onCooldown: journalFailure,
					http: &http.Client{Transport: discordTestTransport(func(*http.Request) (*http.Response, error) {
						return discordTestResponse(429, `{"retry_after":120}`, nil), nil
					})}}
				if _, err := d.check(context.Background(), "abc"); err == nil {
					t.Fatal("journal failure was ignored")
				}
			}
			// This also verifies that the failed request path released its lock.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			lease, err := gate.Begin(ctx, time.Time{}, time.Time{})
			if lease != nil {
				lease.Close()
			}
			var cooldown *searchCooldownError
			if !errors.As(err, &cooldown) {
				t.Fatalf("another process could bypass the server cooldown: %v", err)
			}
		})
	}
}
