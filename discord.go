package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const discordValidationURL = "https://discord.com/api/v9/unique-username/username-attempt-unauthed"

type discordClient struct {
	http          *http.Client
	interval      time.Duration
	last, next    time.Time
	now           func() time.Time
	sleep         func(context.Context, time.Duration) error
	beforeRequest func(time.Time) error
	onCooldown    func(time.Time) error
	gate          *providerGate
}

type discordSession struct {
	file        *os.File
	unlock      func() error
	client      *discordClient
	cache       map[string]verification
	generations map[string]uint64
	published   map[string]uint64
}

type discordControl struct {
	Type           string    `json:"type"`
	LastRequestAt  time.Time `json:"lastRequestAt,omitempty"`
	NextEligibleAt time.Time `json:"nextEligibleAt,omitempty"`
}

func validDiscordName(name string) bool {
	if len(name) < 2 || len(name) > 32 || strings.Contains(name, "..") {
		return false
	}
	for i := range name {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (d *discordClient) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *discordClient) saveCooldown(until time.Time) error {
	if until.After(d.next) {
		d.next = until
	}
	if d.onCooldown != nil {
		if err := d.onCooldown(d.next); err != nil {
			return fmt.Errorf("could not persist Discord cooldown; do not retry before %s: %w", d.next.UTC().Format(time.RFC3339Nano), err)
		}
	}
	return nil
}

func (d *discordClient) check(ctx context.Context, name string) (verification, error) {
	if !validDiscordName(name) {
		return verification{}, errors.New("invalid Discord username: use 2-32 lowercase letters, digits, underscores or periods, without consecutive periods")
	}
	if d.http == nil {
		return verification{}, errors.New("Discord HTTP client is not configured")
	}
	if d.sleep == nil {
		d.sleep = verificationPause
	}
	// Copy the injectable client so redirects and cookie jars cannot transmit
	// credentials, even if a caller supplied a client configured for another site.
	httpClient := *d.http
	httpClient.Jar = nil
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	body, _ := json.Marshal(map[string]string{"username": name})
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return verification{}, err
		}
		if d.gate == nil && d.clock().Before(d.next) {
			return verification{}, cooldownFailure(d.next, "Discord cooldown")
		}
		var lease *providerLease
		if d.gate != nil {
			var err error
			lease, err = d.gate.Begin(ctx, d.last, d.next)
			if err != nil {
				return verification{}, err
			}
		} else if !d.last.IsZero() {
			if delay := d.last.Add(d.interval).Sub(d.clock()); delay > 0 {
				if err := d.sleep(ctx, delay); err != nil {
					return verification{}, err
				}
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, discordValidationURL, bytes.NewReader(body))
		if err != nil {
			closeSearchLease(lease)
			return verification{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "hunt/0.2.0")
		d.last = d.clock()
		if d.beforeRequest != nil {
			if err := d.beforeRequest(d.last); err != nil {
				closeSearchLease(lease)
				return verification{}, fmt.Errorf("could not persist Discord request time: %w", err)
			}
		}
		progressRequest(ctx, "verification")
		response, err := httpClient.Do(req)
		if err != nil {
			if releaseErr := closeSearchLease(lease); releaseErr != nil {
				return verification{}, releaseErr
			}
			if ctx.Err() != nil {
				return verification{}, ctx.Err()
			}
			if attempt < 2 {
				continue
			}
			return verification{}, transientFailure("Discord connection attempts exhausted")
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
		response.Body.Close()
		remainingZero := strings.TrimSpace(response.Header.Get("X-RateLimit-Remaining")) == "0"
		rateLimited := response.StatusCode == http.StatusTooManyRequests || response.Header.Get("Retry-After") != "" || remainingZero
		if rateLimited {
			until := discordCooldown(response.Header, data, d.clock())
			if until.Before(d.next) {
				until = d.next
			}
			if lease != nil {
				if err := lease.Cooldown(until); err != nil {
					closeSearchLease(lease)
					return verification{}, err
				}
			}
			if err := d.saveCooldown(until); err != nil {
				closeSearchLease(lease)
				return verification{}, err
			}
		}
		if err := closeSearchLease(lease); err != nil {
			return verification{}, err
		}
		if response.StatusCode == http.StatusTooManyRequests {
			return verification{}, cooldownFailure(d.next, "Discord rate limit")
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound {
			return verification{}, fmt.Errorf("Discord HTTP %d; availability unknown; no authentication or account action attempted", response.StatusCode)
		}
		if response.StatusCode >= 500 && response.StatusCode <= 599 {
			if searchChallengeResponse(data) {
				return verification{}, errors.New("Discord returned a challenge; availability unknown; search stopped")
			}
			if rateLimited {
				return verification{}, cooldownFailure(d.next, "Discord server response requested cooldown")
			}
			if !rateLimited && attempt < 2 {
				continue
			}
			return verification{}, transientFailure("Discord server attempts exhausted")
		}
		if response.StatusCode == http.StatusBadRequest && readErr == nil && len(data) <= 64*1024 && discordInvalidName(data) {
			return verification{Name: name, Status: "invalid", CheckedAt: d.clock().UTC(), Source: discordValidationURL}, nil
		}
		if response.StatusCode != http.StatusOK {
			return verification{}, fmt.Errorf("unrecognized Discord response (HTTP %d); availability unknown", response.StatusCode)
		}
		if readErr != nil {
			if rateLimited {
				return verification{}, cooldownFailure(d.next, "Discord response requested cooldown")
			}
			if !rateLimited && attempt < 2 {
				continue
			}
			return verification{}, transientFailure("Discord response reads exhausted")
		}
		if len(data) > 64*1024 {
			return verification{}, errors.New("unexpectedly large Discord response; availability unknown")
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal(data, &payload) != nil || payload == nil {
			return verification{}, errors.New("invalid Discord JSON response; availability unknown")
		}
		for _, key := range []string{"error", "errors", "message", "code", "captcha_key", "captcha_sitekey", "captcha_rqdata", "captcha_rqtoken"} {
			if _, found := payload[key]; found {
				return verification{}, errors.New("Discord returned an error or challenge; availability unknown")
			}
		}
		var taken *bool
		if raw, present := payload["taken"]; !present || json.Unmarshal(raw, &taken) != nil || taken == nil {
			return verification{}, errors.New("Discord did not return an explicit taken boolean; availability unknown")
		}
		status := "available"
		if *taken {
			status = "unavailable"
		}
		return verification{Name: name, Status: status, CheckedAt: d.clock().UTC(), Source: discordValidationURL}, nil
	}
	return verification{}, errors.New("Discord request attempts exhausted; availability unknown")
}

func discordInvalidName(body []byte) bool {
	var payload struct {
		Code   int `json:"code"`
		Errors struct {
			Username struct {
				Errors []struct {
					Code string `json:"code"`
				} `json:"_errors"`
			} `json:"username"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Code != 50035 || len(payload.Errors.Username.Errors) == 0 {
		return false
	}
	for _, item := range payload.Errors.Username.Errors {
		if !strings.HasPrefix(item.Code, "USERNAME_INVALID_") {
			return false
		}
	}
	return true
}

func discordCooldown(headers http.Header, body []byte, now time.Time) time.Time {
	var until time.Time
	addRelative := func(value string) {
		seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds > float64(math.MaxInt64)/float64(time.Second) {
			return
		}
		if candidate := now.Add(time.Duration(seconds * float64(time.Second))); candidate.After(until) {
			until = candidate
		}
	}
	addRelative(headers.Get("Retry-After"))
	if parsed, err := http.ParseTime(headers.Get("Retry-After")); err == nil && parsed.After(until) {
		until = parsed
	}
	addRelative(headers.Get("X-RateLimit-Reset-After"))
	if seconds, err := strconv.ParseFloat(headers.Get("X-RateLimit-Reset"), 64); err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds >= 0 && seconds < float64(math.MaxInt64) {
		whole, fraction := math.Modf(seconds)
		if parsed := time.Unix(int64(whole), int64(fraction*float64(time.Second))); parsed.After(until) {
			until = parsed
		}
	}
	var payload struct {
		RetryAfter json.RawMessage `json:"retry_after"`
	}
	if json.Unmarshal(body, &payload) == nil {
		addRelative(string(payload.RetryAfter))
	}
	if until.IsZero() || until.Before(now) {
		return now.Add(time.Minute)
	}
	return until
}

func openDiscordSession(path string, cfg discordConfig) (*discordSession, error) {
	unlock, err := lockJournal(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		unlock()
		return nil, err
	}
	s := &discordSession{file: file, unlock: unlock, cache: make(map[string]verification), generations: make(map[string]uint64), published: make(map[string]uint64), client: &discordClient{http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, interval: time.Second}}
	bound := persistentDiscordConfig(cfg)
	lines := 0
	if err := readJournal(file, func(line []byte, number int) error {
		var entry struct {
			Type string `json:"type"`
			discordConfig
			verification
			LastRequestAt  time.Time `json:"lastRequestAt"`
			NextEligibleAt time.Time `json:"nextEligibleAt"`
		}
		if json.Unmarshal(line, &entry) != nil {
			return fmt.Errorf("invalid Discord journal at line %d", number)
		}
		if lines == 0 {
			if entry.Type != "discord_config" || persistentDiscordConfig(entry.discordConfig) != bound {
				return errors.New("Discord journal configuration differs; reuse its flags or choose a new -out")
			}
			lines++
			return nil
		}
		lines++
		switch entry.Type {
		case "discord_control":
			if entry.LastRequestAt.IsZero() && entry.NextEligibleAt.IsZero() {
				return fmt.Errorf("empty Discord control at line %d", number)
			}
			if entry.LastRequestAt.After(s.client.last) {
				s.client.last = entry.LastRequestAt
			}
			if entry.NextEligibleAt.After(s.client.next) {
				s.client.next = entry.NextEligibleAt
			}
		case "discord_check":
			result := entry.verification
			if !validDiscordName(result.Name) || (result.Status != "available" && result.Status != "unavailable" && result.Status != "invalid") || result.CheckedAt.IsZero() || result.Source != discordValidationURL {
				return fmt.Errorf("invalid Discord check at line %d", number)
			}
			generation := s.generations[result.Name] + 1
			if generation == 0 || (result.Generation != 0 && result.Generation != generation) {
				return fmt.Errorf("invalid Discord observation generation at line %d", number)
			}
			result.Generation = generation
			s.generations[result.Name] = generation
			// The last completed observation wins, even if the local clock was
			// adjusted or two checks share the same timestamp.
			s.cache[result.Name] = result
		case "discord_published":
			if !validDiscordName(entry.Name) || entry.Generation == 0 || entry.Generation != s.generations[entry.Name] || s.cache[entry.Name].Status != "available" {
				return fmt.Errorf("invalid Discord publication acknowledgment at line %d", number)
			}
			s.published[entry.Name] = entry.Generation
		default:
			return fmt.Errorf("unexpected Discord journal record at line %d", number)
		}
		return nil
	}); err != nil {
		s.Close()
		return nil, err
	}
	if lines == 0 {
		if err := s.write(struct {
			Type string `json:"type"`
			discordConfig
		}{"discord_config", bound}); err != nil {
			s.Close()
			return nil, err
		}
	}
	s.client.beforeRequest = func(at time.Time) error {
		return s.write(discordControl{Type: "discord_control", LastRequestAt: at.UTC()})
	}
	s.client.onCooldown = func(until time.Time) error {
		return s.write(discordControl{Type: "discord_control", NextEligibleAt: until.UTC()})
	}
	return s, nil
}

func (s *discordSession) cached(name string) (verification, bool) {
	result, found := s.cache[name]
	if !found {
		return verification{}, false
	}
	ttl := 24 * time.Hour
	if result.Status == "available" {
		ttl = 30 * time.Second
	}
	age := s.client.clock().Sub(result.CheckedAt)
	return result, age >= 0 && age < ttl
}

// recorded exposes the last durable observation for a resumable single pass.
// Callers must not describe an expired positive observation as current.
func (s *discordSession) recorded(name string) (verification, bool) {
	result, found := s.cache[name]
	return result, found
}

func (s *discordSession) check(ctx context.Context, name string, fresh bool) (verification, error) {
	if err := ctx.Err(); err != nil {
		return verification{}, err
	}
	if !validDiscordName(name) {
		return verification{}, errors.New("invalid Discord username")
	}
	if !fresh {
		if result, found := s.cached(name); found {
			return result, nil
		}
	}
	result, err := s.client.check(ctx, name)
	if err != nil {
		return verification{}, err
	}
	result.Generation = s.generations[name] + 1
	if result.Generation == 0 {
		return verification{}, errors.New("Discord observation generation overflow")
	}
	if err := s.write(struct {
		Type string `json:"type"`
		verification
	}{"discord_check", result}); err != nil {
		return verification{}, err
	}
	s.cache[name] = result
	s.generations[name] = result.Generation
	progressVerified(ctx, result)
	return result, nil
}

func (s *discordSession) needsPublish(name string) bool {
	result, found := s.cache[name]
	return found && result.Status == "available" && s.published[name] != result.Generation
}

func (s *discordSession) markPublished(result verification) error {
	current, found := s.cache[result.Name]
	if !found || current.Status != "available" || result.Status != "available" || result.Generation == 0 || result.Generation != current.Generation {
		return errors.New("refusing Discord publication acknowledgment for an unknown or stale observation")
	}
	if s.published[result.Name] == result.Generation {
		return nil
	}
	if err := s.write(publicationAck{Type: "discord_published", Name: result.Name, Generation: result.Generation}); err != nil {
		return err
	}
	s.published[result.Name] = result.Generation
	return nil
}

func (s *discordSession) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := s.file.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return s.file.Sync()
}

func (s *discordSession) Close() error {
	var err error
	if s.file != nil {
		err = s.file.Close()
		s.file = nil
	}
	if s.unlock != nil {
		unlockErr := s.unlock()
		s.unlock = nil
		if err == nil {
			err = unlockErr
		}
	}
	return err
}
