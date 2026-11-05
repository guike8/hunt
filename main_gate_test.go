package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestGraphQLClientPublishesCooldownToIndependentClient(t *testing.T) {
	t.Setenv("HUNT_STATE_DIR", t.TempDir())
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	calls := 0
	transport := finderTestTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return finderTestResponse(http.StatusOK, fmt.Sprintf(`{"data":{"u0":null,"rateLimit":{"remaining":0,"resetAt":%q}}}`, until.Format(time.RFC3339)), nil), nil
	})
	first := finderTestClient(transport)
	first.gate = testProviderGate(t, "github-graphql", 0, nil)
	results, err := first.lookup(context.Background(), []string{"abc"})
	var limited *searchCooldownError
	if !errors.As(err, &limited) || len(results) != 1 || !limited.Until.Equal(until) {
		t.Fatalf("GraphQL result/cooldown lost: results=%v error=%v", results, err)
	}
	second := finderTestClient(transport)
	second.gate = testProviderGate(t, "github-graphql", 0, nil)
	_, err = second.lookup(context.Background(), []string{"def"})
	if !errors.As(err, &limited) || !limited.Until.Equal(until) || calls != 1 {
		t.Fatalf("independent client bypassed shared deadline: calls=%d error=%v", calls, err)
	}
}

func TestGraphQLClientReleasesProviderLeaseBeforeRetry(t *testing.T) {
	t.Setenv("HUNT_STATE_DIR", t.TempDir())
	calls := 0
	client := finderTestClient(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return finderTestResponse(http.StatusServiceUnavailable, "temporary failure", nil), nil
		}
		return finderTestResponse(http.StatusOK, finderTestBody(t, nil), nil), nil
	})
	client.gate = testProviderGate(t, "github-graphql", 0, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results, err := client.lookup(ctx, []string{"abc"})
	if err != nil || calls != 2 || len(results) != 1 {
		t.Fatalf("provider lease was retained across retry: calls=%d results=%v error=%v", calls, results, err)
	}
}
