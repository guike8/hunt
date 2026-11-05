package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeDiscordChecker struct {
	cache   map[string]verification
	results map[string]string
	calls   []string
	fresh   []bool
	err     error
}

func (f *fakeDiscordChecker) cached(name string) (verification, bool) {
	result, ok := f.cache[name]
	return result, ok
}
func (f *fakeDiscordChecker) check(ctx context.Context, name string, fresh bool) (verification, error) {
	f.calls = append(f.calls, name)
	f.fresh = append(f.fresh, fresh)
	if f.err != nil {
		return verification{}, f.err
	}
	return verification{Name: name, Status: f.results[name]}, nil
}

func TestDiscordResumeUsesCacheAndCountsOnlyNewChecks(t *testing.T) {
	fake := &fakeDiscordChecker{cache: map[string]verification{
		"abc": {Name: "abc", Status: "unavailable"}, "def": {Name: "def", Status: "available"},
	}, results: map[string]string{"ghi": "available", "jkl": "available"}}
	var out, log bytes.Buffer
	err := runDiscordChecks(context.Background(), fake, []string{"abc", "def", "ghi", "jkl"}, discordOptions{N: 1, Find: 2, Delay: time.Second}, &out, &log)
	if err != nil || out.String() != "def\nghi\n" || !reflect.DeepEqual(fake.calls, []string{"ghi"}) {
		t.Fatalf("error=%v out=%q calls=%v", err, out.String(), fake.calls)
	}
}

func TestDiscordFreshBypassesCacheAndStopsAtFindTarget(t *testing.T) {
	fake := &fakeDiscordChecker{cache: map[string]verification{"abc": {Name: "abc", Status: "unavailable"}, "def": {Name: "def", Status: "available"}}, results: map[string]string{"abc": "available", "def": "unavailable"}}
	var out bytes.Buffer
	err := runDiscordChecks(context.Background(), fake, []string{"abc", "def"}, discordOptions{Fresh: true, Find: 1, Delay: time.Second}, &out, io.Discard)
	if err != nil || out.String() != "abc\n" || !reflect.DeepEqual(fake.calls, []string{"abc"}) || !reflect.DeepEqual(fake.fresh, []bool{true}) {
		t.Fatalf("error=%v out=%q calls=%v fresh=%v", err, out.String(), fake.calls, fake.fresh)
	}
}

func TestDiscordUnknownOrRequestErrorNeverPrintsAvailability(t *testing.T) {
	for _, err := range []error{nil, errors.New("rate limited")} {
		fake := &fakeDiscordChecker{results: map[string]string{"abc": "unknown"}, err: err}
		var out bytes.Buffer
		if got := runDiscordChecks(context.Background(), fake, []string{"abc", "def"}, discordOptions{}, &out, io.Discard); got == nil || out.Len() != 0 || len(fake.calls) != 1 {
			t.Fatalf("error=%v out=%q calls=%v", got, out.String(), fake.calls)
		}
	}
}

func TestDiscordExplicitInvalidContinuesWithoutAvailability(t *testing.T) {
	fake := &fakeDiscordChecker{results: map[string]string{"abc": "invalid", "def": "available"}}
	var out, log bytes.Buffer
	err := runDiscordChecks(context.Background(), fake, []string{"abc", "def"}, discordOptions{Find: 1}, &out, &log)
	if err != nil || out.String() != "def\n" || len(fake.calls) != 2 || !strings.Contains(log.String(), "invalid name: abc") {
		t.Fatalf("error=%v out=%q calls=%v log=%q", err, out.String(), fake.calls, log.String())
	}
}

type historyDiscordChecker struct {
	*fakeDiscordChecker
	history map[string]verification
}

func (f *historyDiscordChecker) recorded(name string) (verification, bool) {
	result, ok := f.history[name]
	return result, ok
}

func TestDiscordWaitResumeRetainsOldNegativesButRechecksOldAvailability(t *testing.T) {
	fake := &historyDiscordChecker{
		fakeDiscordChecker: &fakeDiscordChecker{results: map[string]string{"def": "available"}},
		history: map[string]verification{
			"abc": {Name: "abc", Status: "unavailable", CheckedAt: time.Now().Add(-48 * time.Hour)},
			"def": {Name: "def", Status: "available", CheckedAt: time.Now().Add(-48 * time.Hour)},
		},
	}
	var out bytes.Buffer
	err := runDiscordChecks(context.Background(), fake, []string{"abc", "def"}, discordOptions{Resume: true, Find: 1}, &out, io.Discard)
	if err != nil || out.String() != "def\n" || !reflect.DeepEqual(fake.calls, []string{"def"}) {
		t.Fatalf("wait resume repeated negative or emitted stale availability: error=%v out=%q requests=%v", err, out.String(), fake.calls)
	}
}

func TestDiscordRunnerRequiresNoGitHubAuthentication(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", "")
	old := http.DefaultTransport
	calls := 0
	http.DefaultTransport = sourceTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "discord.com" || req.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected service/credentials: %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"taken":false}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	cfg, err := configureDiscordSource(discordConfig{Version: 1, Platform: "discord", Length: 2, Alphabet: "abcdefghijklmnopqrstuvwxyz", Seed: 1}, "", "ab")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = runDiscord(context.Background(), discordOptions{Config: cfg, N: 1, Out: filepath.Join(t.TempDir(), "discord.jsonl"), Delay: time.Second}, &out, io.Discard)
	if err != nil || calls != 1 || out.String() != "ab\n" {
		t.Fatalf("error=%v calls=%d out=%q", err, calls, out.String())
	}
}

type discordOutputFailure struct{}

func (discordOutputFailure) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func TestDiscordOutputFailureRemainsPendingAndPublishedResumeAdvances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discord.jsonl")
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	s, err := openDiscordSession(path, discordTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	discordTestClock(s.client, &now)
	calls := 0
	s.client.http.Transport = discordTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return discordTestResponse(200, `{"taken":false}`, nil), nil
	})
	opts := discordOptions{Resume: true, N: 1}
	if err := runDiscordChecks(context.Background(), s, []string{"abc"}, opts, discordOutputFailure{}, io.Discard); err == nil || !s.needsPublish("abc") {
		t.Fatalf("failed stdout was acknowledged: err=%v pending=%t", err, s.needsPublish("abc"))
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
	var out bytes.Buffer
	if err := runDiscordChecks(context.Background(), s, []string{"abc"}, opts, &out, io.Discard); err != nil || out.String() != "abc\n" || calls != 1 || s.needsPublish("abc") {
		t.Fatalf("fresh pending result was lost or rechecked: err=%v out=%q calls=%d", err, out.String(), calls)
	}
	// After availability expires, a delivered positive is already complete
	// for this pass. The one-request budget must reach the next name.
	now = now.Add(time.Hour)
	out.Reset()
	if err := runDiscordChecks(context.Background(), s, []string{"abc", "def"}, opts, &out, io.Discard); err != nil || out.Len() != 0 || calls != 2 {
		t.Fatalf("published positive consumed the next pass: err=%v out=%q calls=%d", err, out.String(), calls)
	}
	if latest, found := s.recorded("def"); !found || latest.Status != "unavailable" {
		t.Fatalf("resume did not advance beyond old availability: %#v found=%t", latest, found)
	}
}

func TestDiscordStalePendingPositiveMustBeReverifiedBeforePublishing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discord.jsonl")
	s, err := openDiscordSession(path, discordTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	if err := s.write(struct {
		Type string `json:"type"`
		verification
	}{"discord_check", verification{Name: "abc", Status: "available", CheckedAt: old, Source: discordValidationURL}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = openDiscordSession(path, discordTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := old.Add(time.Hour)
	discordTestClock(s.client, &now)
	calls := 0
	s.client.http.Transport = discordTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return discordTestResponse(200, `{"taken":true}`, nil), nil
	})
	var out bytes.Buffer
	if err := runDiscordChecks(context.Background(), s, []string{"abc"}, discordOptions{Resume: true}, &out, io.Discard); err != nil || calls != 1 || out.Len() != 0 || s.needsPublish("abc") {
		t.Fatalf("stale pending positive was falsely emitted: err=%v calls=%d out=%q", err, calls, out.String())
	}
}
