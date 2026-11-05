package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWaitNeverEnablesMutationRetriesOrFreshRescans(t *testing.T) {
	for _, tc := range []struct{ wait, claim, fresh, invalid bool }{
		{false, false, false, false},
		{false, true, false, false},
		{false, false, true, false},
		{true, false, false, false},
		{true, true, false, true},
		{true, false, true, true},
	} {
		if err := validateWaitFlags(tc.wait, tc.claim, tc.fresh); (err != nil) != tc.invalid {
			t.Fatalf("wait=%v claim=%v fresh=%v error=%v", tc.wait, tc.claim, tc.fresh, err)
		}
	}
}

func TestGraphQLCooldownUsesLatestServerDeadline(t *testing.T) {
	latest := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	headers := http.Header{"Retry-After": {"60"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(latest.Unix(), 10)}}
	if got := graphQLCooldownUntil(headers, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); !got.Equal(latest) {
		t.Fatalf("deadline=%s want=%s", got, latest)
	}
	start := time.Now()
	if got := graphQLCooldownUntil(nil, ""); got.Before(start.Add(time.Minute)) || got.After(time.Now().Add(time.Minute)) {
		t.Fatalf("missing deadline has no bounded fallback: %s", got)
	}
}

func TestGraphQLRateLimitsAreTypedButAuthenticationIsNot(t *testing.T) {
	for _, body := range []string{
		`{"data":{"u0":null,"rateLimit":{"remaining":0,"resetAt":"2030-01-01T00:00:00Z"}}}`,
		`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit reached"}]}`,
	} {
		_, err := parseResponse([]byte(body), []string{"abc"}, nil)
		var cooldown *searchCooldownError
		if !errors.As(err, &cooldown) {
			t.Fatalf("rate response did not preserve typed cooldown: %v", err)
		}
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		g := &githubClient{http: &http.Client{Transport: sourceTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})}}
		_, err := g.lookup(context.Background(), []string{"abc"})
		var cooldown *searchCooldownError
		if err == nil || errors.As(err, &cooldown) {
			t.Fatalf("HTTP %d authentication/denial became retryable: %v", status, err)
		}
	}
}
