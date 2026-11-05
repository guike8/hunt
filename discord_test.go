package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type discordTestTransport func(*http.Request) (*http.Response, error)

func (f discordTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func discordTestResponse(code int, body string, headers http.Header) *http.Response {
	if headers == nil {
		headers = make(http.Header)
	}
	return &http.Response{StatusCode: code, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
}

func discordTestConfig() discordConfig {
	return discordConfig{Version: 1, Platform: "discord", Length: 3, Alphabet: "abcdefghijklmnopqrstuvwxyz", Seed: 1}
}

func discordTestClock(d *discordClient, now *time.Time) {
	d.now = func() time.Time { return *now }
	d.sleep = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		*now = now.Add(delay)
		return nil
	}
}

func TestValidDiscordNameBoundaries(t *testing.T) {
	for _, name := range []string{"ab", ".a", "a.", "__", "a_b.c", strings.Repeat("a", 32)} {
		if !validDiscordName(name) {
			t.Fatalf("valid name rejected: %q", name)
		}
	}
	for _, name := range []string{"", "a", "..", "a..b", "AB", "a-b", "a b", "a&x", "aé", strings.Repeat("a", 33)} {
		if validDiscordName(name) {
			t.Fatalf("invalid name accepted: %q", name)
		}
	}
}

func TestDiscordStrictBooleanAndErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body, status string
		code               int
	}{
		{"available", `{"taken":false}`, "available", 200},
		{"unavailable", `{"taken":true}`, "unavailable", 200},
		{"missing", `{}`, "", 200},
		{"null", `{"taken":null}`, "", 200},
		{"string", `{"taken":"false"}`, "", 200},
		{"number", `{"taken":0}`, "", 200},
		{"array", `[]`, "", 200},
		{"html", `<html>available</html>`, "", 200},
		{"malformed", `{"taken":false`, "", 200},
		{"oversized", `{"taken":false,"padding":"` + strings.Repeat("x", 64*1024) + `"}`, "", 200},
		{"error", `{"taken":false,"message":"try again"}`, "", 200},
		{"challenge", `{"taken":false,"captcha_key":["captcha-required"]}`, "", 200},
		{"auth", `{"taken":false}`, "", 401},
		{"forbidden", `{"taken":false}`, "", 403},
		{"notFound", `{"taken":false}`, "", 404},
		{"invalidName", `{"code":50035,"errors":{"username":{"_errors":[{"code":"USERNAME_INVALID_CONTAINS","message":"invalid"}]}}}`, "invalid", 400},
		{"unknownFormError", `{"code":50035,"errors":{"username":{"_errors":[{"code":"UNKNOWN_VALIDATION_ERROR"}]}}}`, "", 400},
		{"genericBadRequest", `{"code":50035,"message":"invalid form"}`, "", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := discordClient{http: &http.Client{Transport: discordTestTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return discordTestResponse(tc.code, tc.body, nil), nil
			})}}
			result, err := d.check(context.Background(), "abc")
			if result.Status != tc.status || (err == nil) != (tc.status != "") || calls != 1 {
				t.Fatalf("result=%#v err=%v calls=%d; want %q in one request", result, err, calls, tc.status)
			}
			if tc.status != "" && (result.Name != "abc" || result.Source != discordValidationURL || result.CheckedAt.IsZero()) {
				t.Fatalf("missing result provenance: %#v", result)
			}
		})
	}
}

func TestDiscordNeverSendsTokensCookiesOrFollowsRedirects(t *testing.T) {
	t.Setenv("GH_TOKEN", "github-secret")
	t.Setenv("GITHUB_TOKEN", "other-github-secret")
	t.Setenv("DISCORD_TOKEN", "discord-secret")
	jar, _ := cookiejar.New(nil)
	endpoint, _ := url.Parse(discordValidationURL)
	jar.SetCookies(endpoint, []*http.Cookie{{Name: "secret_cookie", Value: "secret"}})
	calls := 0
	d := discordClient{http: &http.Client{Jar: jar, Transport: discordTestTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodPost || req.URL.String() != discordValidationURL || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
			t.Fatal("wrong endpoint, method, or credentials transmitted")
		}
		var payload map[string]string
		if json.NewDecoder(req.Body).Decode(&payload) != nil || !reflect.DeepEqual(payload, map[string]string{"username": "abc"}) {
			t.Fatalf("unexpected request body: %#v", payload)
		}
		return discordTestResponse(302, `{"taken":false}`, http.Header{"Location": []string{"https://example.com/redirect"}}), nil
	})}}
	if result, err := d.check(context.Background(), "abc"); err == nil || result.Status != "" || calls != 1 {
		t.Fatalf("redirect followed or treated as available: %#v err=%v calls=%d", result, err, calls)
	}
}

func TestDiscordCooldownUsesLatestFractionalExpiry(t *testing.T) {
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	headers := http.Header{
		"Retry-After":             []string{"0.25"},
		"X-Ratelimit-Reset-After": []string{"1.75"},
		"X-Ratelimit-Reset":       []string{strconv.FormatFloat(float64(now.Unix())+2.5, 'f', 1, 64)},
	}
	until := discordCooldown(headers, []byte(`{"retry_after":0.5}`), now)
	if !until.Equal(now.Add(2500 * time.Millisecond)) {
		t.Fatalf("fractional maximum lost: got %s", until)
	}
	until = discordCooldown(http.Header{"Retry-After": []string{now.Add(3 * time.Second).Format(http.TimeFormat)}}, []byte(`{"retry_after":0.5}`), now)
	if !until.Equal(now.Add(3 * time.Second)) {
		t.Fatalf("HTTP-date cooldown ignored: %s", until)
	}
	if until := discordCooldown(nil, []byte(`{"retry_after":0.5}`), now); !until.Equal(now.Add(500 * time.Millisecond)) {
		t.Fatalf("fractional JSON cooldown ignored: %s", until)
	}
	if until := discordCooldown(nil, []byte(`not json`), now); !until.Equal(now.Add(time.Minute)) {
		t.Fatalf("missing cooldown did not use fallback: %s", until)
	}
}

func TestDiscordSessionRateCooldownAndSuccessfulExhaustionSurviveRestart(t *testing.T) {
	for _, code := range []int{200, 429} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "discord.jsonl")
			now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
			calls := 0
			s, err := openDiscordSession(path, discordTestConfig())
			if err != nil {
				t.Fatal(err)
			}
			discordTestClock(s.client, &now)
			s.client.http.Transport = discordTestTransport(func(*http.Request) (*http.Response, error) {
				calls++
				body := `{"taken":false}`
				if code == 429 {
					body = `{"retry_after":2.5}`
				}
				return discordTestResponse(code, body, http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset-After": []string{"2.5"}}), nil
			})
			result, err := s.check(context.Background(), "abc", false)
			if code == 200 && (err != nil || result.Status != "available") {
				t.Fatalf("successful exhausted response lost: %#v %v", result, err)
			}
			if code == 429 && (err == nil || result.Status != "") {
				t.Fatalf("429 classified as a result: %#v %v", result, err)
			}
			if _, err := s.check(context.Background(), "def", true); err == nil || calls != 1 {
				t.Fatalf("active cooldown ignored: err=%v calls=%d", err, calls)
			}
			s.Close()
			now = now.Add(time.Second)
			s, err = openDiscordSession(path, discordTestConfig())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			discordTestClock(s.client, &now)
			s.client.http.Transport = discordTestTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return discordTestResponse(200, `{"taken":true}`, nil), nil
			})
			if code == 200 {
				if cached, ok := s.cached("abc"); !ok || cached.Status != "available" {
					t.Fatal("conclusive exhausted result was not persisted")
				}
			}
			if _, err := s.check(context.Background(), "def", true); err == nil || calls != 1 {
				t.Fatalf("restart bypassed cooldown: err=%v calls=%d", err, calls)
			}
			now = now.Add(2 * time.Second)
			if result, err := s.check(context.Background(), "def", true); err != nil || result.Status != "unavailable" || calls != 2 {
				t.Fatalf("expired cooldown failed: %#v err=%v calls=%d", result, err, calls)
			}
		})
	}
}

func TestDiscordSessionCacheFreshnessAndRestartPacing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discord.jsonl")
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	start := now
	calls := 0
	s, err := openDiscordSession(path, discordTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	discordTestClock(s.client, &now)
	s.client.http.Transport = discordTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return discordTestResponse(200, `{"taken":false}`, nil), nil
	})
	if _, err := s.check(context.Background(), "abc", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.check(context.Background(), "abc", false); err != nil || calls != 1 {
		t.Fatalf("fresh cache was not used: err=%v calls=%d", err, calls)
	}
	s.Close()
	s, err = openDiscordSession(path, discordTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	discordTestClock(s.client, &now)
	s.client.http.Transport = discordTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return discordTestResponse(200, `{"taken":true}`, nil), nil
	})
	if _, err := s.check(context.Background(), "abc", true); err != nil || calls != 2 || now.Sub(start) < time.Second {
		t.Fatalf("fresh refresh bypassed restart cadence: err=%v calls=%d elapsed=%s", err, calls, now.Sub(start))
	}
	now = now.Add(23 * time.Hour)
	if _, err := s.check(context.Background(), "abc", false); err != nil || calls != 2 {
		t.Fatalf("negative TTL not cached: err=%v calls=%d", err, calls)
	}
	now = now.Add(2 * time.Hour)
	if _, err := s.check(context.Background(), "abc", false); err != nil || calls != 3 {
		t.Fatalf("negative TTL did not expire: err=%v calls=%d", err, calls)
	}
}

func TestDiscordCachedTTLBoundaries(t *testing.T) {
	for _, tc := range []struct {
		status string
		ttl    time.Duration
	}{
		{"available", 30 * time.Second},
		{"unavailable", 24 * time.Hour},
		{"invalid", 24 * time.Hour},
	} {
		t.Run(tc.status, func(t *testing.T) {
			now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
			s := discordSession{client: &discordClient{}, cache: map[string]verification{"abc": {Name: "abc", Status: tc.status, CheckedAt: now, Source: discordValidationURL}}}
			discordTestClock(s.client, &now)
			now = now.Add(tc.ttl - time.Millisecond)
			if _, ok := s.cached("abc"); !ok {
				t.Fatal("fresh cache entry rejected")
			}
			now = now.Add(time.Millisecond)
			if _, ok := s.cached("abc"); ok {
				t.Fatal("expired cache entry accepted")
			}
		})
	}
}

func TestDiscordRetriesAreBoundedAndPaced(t *testing.T) {
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	var starts []time.Time
	d := discordClient{interval: 2 * time.Second, http: &http.Client{}}
	discordTestClock(&d, &now)
	d.http.Transport = discordTestTransport(func(*http.Request) (*http.Response, error) {
		starts = append(starts, now)
		if len(starts) == 1 {
			return nil, errors.New("temporary connection failure")
		}
		if len(starts) == 2 {
			return discordTestResponse(503, `unavailable`, nil), nil
		}
		return discordTestResponse(200, `{"taken":false}`, nil), nil
	})
	if result, err := d.check(context.Background(), "abc"); err != nil || result.Status != "available" || len(starts) != 3 {
		t.Fatalf("unexpected retry result: %#v err=%v attempts=%d", result, err, len(starts))
	}
	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < 2*time.Second {
			t.Fatal("retry violated global cadence")
		}
	}
	starts = nil
	d.http.Transport = discordTestTransport(func(*http.Request) (*http.Response, error) {
		starts = append(starts, now)
		return nil, errors.New("persistent network failure")
	})
	if result, err := d.check(context.Background(), "abc"); err == nil || result.Status != "" || len(starts) != 3 {
		t.Fatalf("network retries were not bounded: %#v err=%v attempts=%d", result, err, len(starts))
	}
}

func TestDiscordExhaustedTransientFailuresAreTyped(t *testing.T) {
	for _, mode := range []string{"connection", "server", "read", "challenge", "serverChallenge", "malformed", "auth"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			d := discordClient{sleep: func(context.Context, time.Duration) error { return nil }, http: &http.Client{Transport: discordTestTransport(func(*http.Request) (*http.Response, error) {
				calls++
				if mode == "connection" {
					return nil, errors.New("private proxy diagnostic")
				}
				code, body := 200, `not json`
				if mode == "server" {
					code = 503
				}
				if mode == "auth" {
					code = 401
				}
				if mode == "challenge" {
					body = `{"taken":false,"captcha_key":["challenge"]}`
				}
				if mode == "serverChallenge" {
					code, body = 503, `{"captcha_key":["challenge"]}`
				}
				response := discordTestResponse(code, body, nil)
				if mode == "read" {
					response.Body = verificationReadFailure{}
				}
				return response, nil
			})}}
			result, err := d.check(context.Background(), "abc")
			var transient *searchTransientError
			want := mode == "connection" || mode == "server" || mode == "read"
			if result.Status != "" || err == nil || errors.As(err, &transient) != want || (want && calls != 3) || (!want && calls != 1) {
				t.Fatalf("wrong transient classification: mode=%s result=%#v err=%v calls=%d", mode, result, err, calls)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("private diagnostics leaked")
			}
		})
	}
}

func TestDiscordJournalRecoveryConfigurationAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discord.jsonl")
	cfg := discordTestConfig()
	s, err := openDiscordSession(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := openDiscordSession(path, cfg); err == nil {
		other.Close()
		t.Fatal("second writer accepted")
	}
	s.Close()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"torn`)
	f.Close()
	s, err = openDiscordSession(path, cfg)
	if err != nil {
		t.Fatalf("partial tail was not repaired: %v", err)
	}
	s.Close()
	cfg.Seed++
	if other, err := openDiscordSession(path, cfg); err == nil {
		other.Close()
		t.Fatal("mismatched Discord source configuration accepted")
	}
}

func TestDiscordJournalKeepsLatestObservationWithEqualTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discord.jsonl")
	s, err := openDiscordSession(path, discordTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	for _, status := range []string{"available", "unavailable"} {
		if err := s.write(struct {
			Type string `json:"type"`
			verification
		}{"discord_check", verification{Name: "abc", Status: status, CheckedAt: now, Source: discordValidationURL}}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = openDiscordSession(path, discordTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	discordTestClock(s.client, &now)
	if result, found := s.cached("abc"); !found || result.Status != "unavailable" {
		t.Fatalf("replay restored an older observation: %#v found=%t", result, found)
	}
}

func TestDiscordPublicationGenerationSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discord.jsonl")
	s, err := openDiscordSession(path, discordTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	discordTestClock(s.client, &now)
	s.client.interval = 0
	s.client.http.Transport = discordTestTransport(func(*http.Request) (*http.Response, error) {
		return discordTestResponse(200, `{"taken":false}`, nil), nil
	})
	first, err := s.check(context.Background(), "abc", true)
	if err != nil || first.Generation != 1 || !s.needsPublish("abc") {
		t.Fatalf("first observation: %#v err=%v", first, err)
	}
	if err := s.markPublished(first); err != nil {
		t.Fatal(err)
	}
	second, err := s.check(context.Background(), "abc", true)
	if err != nil || second.Generation != 2 || !first.CheckedAt.Equal(second.CheckedAt) || !s.needsPublish("abc") {
		t.Fatalf("equal-time observation reused its acknowledgment: %#v err=%v", second, err)
	}
	if s.markPublished(first) == nil {
		t.Fatal("stale observation acknowledged the latest generation")
	}
	s.Close()
	s, err = openDiscordSession(path, discordTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !s.needsPublish("abc") || s.markPublished(second) != nil {
		t.Fatal("unpublished generation did not survive restart")
	}
	s.Close()
	s, err = openDiscordSession(path, discordTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.needsPublish("abc") {
		t.Fatal("publication acknowledgment did not survive restart")
	}
}

func TestDiscordRejectsInvalidPublicationAcknowledgments(t *testing.T) {
	for _, generation := range []uint64{0, 2} {
		t.Run(strconv.FormatUint(generation, 10), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "discord.jsonl")
			s, err := openDiscordSession(path, discordTestConfig())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.write(struct {
				Type string `json:"type"`
				verification
			}{"discord_check", verification{Name: "abc", Status: "available", CheckedAt: time.Now().UTC(), Source: discordValidationURL}}); err != nil {
				t.Fatal(err)
			}
			if err := s.write(publicationAck{Type: "discord_published", Name: "abc", Generation: generation}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			if reopened, err := openDiscordSession(path, discordTestConfig()); err == nil {
				reopened.Close()
				t.Fatal("invalid or future acknowledgment generation was accepted")
			}
		})
	}
}
