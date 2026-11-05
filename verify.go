package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const validationURL = "https://github.com/signup_check_new/username"

var markup = regexp.MustCompile(`<[^>]*>`)
var shortName = regexp.MustCompile(`^[a-z0-9]{3,4}$`)

type verification struct {
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	CheckedAt  time.Time `json:"checkedAt"`
	Source     string    `json:"source"`
	Generation uint64    `json:"generation,omitempty"`
}

type webVerifier struct {
	client        *http.Client
	interval      time.Duration
	last          time.Time
	cooldown      time.Time
	sleep         func(context.Context, time.Duration) error
	now           func() time.Time
	beforeRequest func(time.Time) error
	onCooldown    func(time.Time) error
	gate          *providerGate
}

type verificationSession struct {
	file        *os.File
	unlock      func() error
	verifier    *webVerifier
	cache       map[string]verification
	force       bool
	generations map[string]uint64
	published   map[string]uint64
}

type publicationAck struct {
	Type       string `json:"type"`
	Name       string `json:"name"`
	Generation uint64 `json:"generation"`
}

type verificationControl struct {
	Type          string    `json:"type"`
	LastRequestAt time.Time `json:"lastRequestAt,omitempty"`
	CooldownUntil time.Time `json:"cooldownUntil,omitempty"`
}

func (v *webVerifier) clock() time.Time {
	if v.now != nil {
		return v.now()
	}
	return time.Now()
}

func verificationPause(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (v *webVerifier) check(ctx context.Context, name string) (string, error) {
	if !shortName.MatchString(name) {
		return "", fmt.Errorf("invalid three- or four-character name %q", name)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if v.gate == nil && v.clock().Before(v.cooldown) {
		return "", cooldownFailure(v.cooldown, "GitHub verification cooldown")
	}
	if v.sleep == nil {
		v.sleep = verificationPause
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var lease *providerLease
		if v.gate != nil {
			var err error
			lease, err = v.gate.Begin(ctx, v.last, v.cooldown)
			if err != nil {
				return "", err
			}
		} else if !v.last.IsZero() {
			if delay := v.last.Add(v.interval).Sub(v.clock()); delay > 0 {
				if err := v.sleep(ctx, delay); err != nil {
					return "", err
				}
			}
		}
		// This is an undocumented, read-only UI endpoint. Never send an API
		// token, browser cookie, or a rename request. Unknown responses stop.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, validationURL+"?value="+url.QueryEscape(name), nil)
		if err != nil {
			closeSearchLease(lease)
			return "", err
		}
		req.Header.Set("Accept", "text/fragment+html")
		req.Header.Set("User-Agent", "github-finder/2.0")
		v.last = v.clock()
		if v.beforeRequest != nil {
			if err := v.beforeRequest(v.last); err != nil {
				closeSearchLease(lease)
				return "", fmt.Errorf("could not persist verification request time: %w", err)
			}
		}
		progressRequest(ctx, "verification")
		response, err := v.client.Do(req)
		if err != nil {
			if releaseErr := closeSearchLease(lease); releaseErr != nil {
				return "", releaseErr
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if attempt < 2 {
				continue
			}
			return "", transientFailure("GitHub verification connection attempts exhausted")
		}
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusForbidden || response.Header.Get("Retry-After") != "" {
			response.Body.Close()
			v.cooldown = verificationCooldown(response.Header.Get("Retry-After"), v.clock())
			if lease != nil {
				if err := lease.Cooldown(v.cooldown); err != nil {
					closeSearchLease(lease)
					return "", err
				}
			}
			if v.onCooldown != nil {
				if err := v.onCooldown(v.cooldown); err != nil {
					closeSearchLease(lease)
					return "", fmt.Errorf("web verification stopped (HTTP %d); do not retry before %s; could not persist cooldown: %w", response.StatusCode, v.cooldown.UTC().Format(time.RFC3339), err)
				}
			}
			if err := closeSearchLease(lease); err != nil {
				return "", err
			}
			if response.StatusCode == http.StatusForbidden && response.Header.Get("Retry-After") == "" {
				return "", errors.New("GitHub verification was denied (HTTP 403); availability unknown; search stopped")
			}
			return "", cooldownFailure(v.cooldown, "GitHub verification rate limit")
		}
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusUnprocessableEntity && response.StatusCode < 500 {
			response.Body.Close()
			if err := closeSearchLease(lease); err != nil {
				return "", err
			}
			return "", fmt.Errorf("unrecognized verification response (HTTP %d); availability unknown", response.StatusCode)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 128*1024+1))
		response.Body.Close()
		if err := closeSearchLease(lease); err != nil {
			return "", err
		}
		if response.StatusCode >= 500 && response.StatusCode <= 599 {
			if searchChallengeResponse(body) {
				return "", errors.New("GitHub verification returned a challenge; availability unknown; search stopped")
			}
			if attempt < 2 {
				continue
			}
			return "", transientFailure("GitHub verification server attempts exhausted")
		}
		if readErr != nil {
			if attempt < 2 {
				continue
			}
			return "", transientFailure("GitHub verification response reads exhausted")
		}
		if len(body) > 128*1024 {
			return "", errors.New("unexpectedly large verification response; availability unknown")
		}
		text := strings.Join(strings.Fields(html.UnescapeString(markup.ReplaceAllString(string(body), " "))), " ")
		if response.StatusCode == http.StatusOK && text == name+" is available." {
			return "available", nil
		}
		// Suggestions can contain "available"; only the requested name's
		// explicit rejection counts as unavailable.
		if response.StatusCode == http.StatusUnprocessableEntity && explicitGitHubUnavailable(text, name) {
			return "unavailable", nil
		}
		return "", fmt.Errorf("unrecognized verification response (HTTP %d); availability unknown; endpoint may have changed", response.StatusCode)
	}
	return "", errors.New("verification attempts exhausted; availability unknown")
}

func closeSearchLease(lease *providerLease) error {
	if lease == nil {
		return nil
	}
	return lease.Close()
}

func explicitGitHubUnavailable(text, name string) bool {
	for _, subject := range []string{name, "'" + name + "'", `"` + name + `"`} {
		for _, ending := range []string{" is not available.", " is unavailable."} {
			message := "Username " + subject + ending
			if text == message || strings.HasPrefix(text, message+" ") {
				return true
			}
		}
	}
	return false
}

func verificationCooldown(value string, now time.Time) time.Time {
	until := now.Add(time.Minute)
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
		if parsed := now.Add(time.Duration(seconds) * time.Second); parsed.After(until) {
			until = parsed
		}
	} else if parsed, err := http.ParseTime(value); err == nil && parsed.After(until) {
		until = parsed
	}
	return until
}

func openVerificationSession(path string) (*verificationSession, error) {
	unlock, err := lockJournal(path)
	if err != nil {
		return nil, err
	}
	// The exclusive lock and readJournal's final Seek provide append safety.
	// O_APPEND denies Truncate on Windows and prevents torn-tail recovery.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		unlock()
		return nil, err
	}
	s := &verificationSession{file: file, unlock: unlock, cache: make(map[string]verification), generations: make(map[string]uint64), published: make(map[string]uint64), verifier: &webVerifier{client: &http.Client{Timeout: 15 * time.Second}, interval: 7 * time.Second}}
	if err := readJournal(file, func(line []byte, lineNo int) error {
		var control verificationControl
		if json.Unmarshal(line, &control) != nil {
			return fmt.Errorf("invalid verification journal at line %d", lineNo)
		}
		if control.Type == "control" {
			if control.LastRequestAt.IsZero() && control.CooldownUntil.IsZero() {
				return fmt.Errorf("empty verification control at line %d", lineNo)
			}
			if control.LastRequestAt.After(s.verifier.last) {
				s.verifier.last = control.LastRequestAt
			}
			if control.CooldownUntil.After(s.verifier.cooldown) {
				s.verifier.cooldown = control.CooldownUntil
			}
			return nil
		}
		if control.Type == "published" {
			var ack publicationAck
			if json.Unmarshal(line, &ack) != nil || !shortName.MatchString(ack.Name) || ack.Generation == 0 || ack.Generation != s.generations[ack.Name] || s.cache[ack.Name].Status != "available" {
				return fmt.Errorf("invalid publication acknowledgment at line %d", lineNo)
			}
			s.published[ack.Name] = ack.Generation
			return nil
		}
		var saved verification
		if control.Type != "" || json.Unmarshal(line, &saved) != nil || !shortName.MatchString(saved.Name) || (saved.Status != "available" && saved.Status != "unavailable") || saved.CheckedAt.IsZero() || saved.Source != validationURL {
			return fmt.Errorf("invalid verification journal at line %d", lineNo)
		}
		generation := s.generations[saved.Name] + 1
		if generation == 0 || (saved.Generation != 0 && saved.Generation != generation) {
			return fmt.Errorf("invalid verification generation at line %d", lineNo)
		}
		saved.Generation = generation
		s.generations[saved.Name] = generation
		// Journal order, not wall-clock timestamps, defines the latest result.
		s.cache[saved.Name] = saved
		// Older journals did not save request starts; their result time is a
		// conservative cadence checkpoint when upgrading.
		if saved.CheckedAt.After(s.verifier.last) {
			s.verifier.last = saved.CheckedAt
		}
		return nil
	}); err != nil {
		s.Close()
		return nil, err
	}
	s.verifier.beforeRequest = func(at time.Time) error {
		return s.write(verificationControl{Type: "control", LastRequestAt: at.UTC()})
	}
	s.verifier.onCooldown = func(until time.Time) error {
		return s.write(verificationControl{Type: "control", CooldownUntil: until.UTC()})
	}
	return s, nil
}

func (s *verificationSession) write(value any) error {
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

func (s *verificationSession) verify(ctx context.Context, name string) (verification, error) {
	if err := ctx.Err(); err != nil {
		return verification{}, err
	}
	if !shortName.MatchString(name) {
		return verification{}, fmt.Errorf("invalid three- or four-character name %q", name)
	}
	now := s.verifier.clock()
	if saved, ok := s.cache[name]; ok && !s.force {
		ttl := 24 * time.Hour
		if saved.Status == "available" {
			ttl = 30 * time.Second
		}
		if age := now.Sub(saved.CheckedAt); age >= 0 && age < ttl {
			return saved, nil
		}
	}
	status, err := s.verifier.check(ctx, name)
	if err != nil {
		return verification{}, err
	}
	result := verification{Name: name, Status: status, CheckedAt: s.verifier.clock().UTC(), Source: validationURL, Generation: s.generations[name] + 1}
	if result.Generation == 0 {
		return verification{}, errors.New("verification generation overflow")
	}
	if err := s.write(result); err != nil {
		return verification{}, err
	}
	s.cache[name] = result
	s.generations[name] = result.Generation
	progressVerified(ctx, result)
	return result, nil
}

func (s *verificationSession) needsPublish(name string) bool {
	result, found := s.cache[name]
	return found && result.Status == "available" && s.published[name] != result.Generation
}

func (s *verificationSession) markPublished(result verification) error {
	current, found := s.cache[result.Name]
	if !found || current.Status != "available" || result.Status != "available" || result.Generation == 0 || result.Generation != current.Generation {
		return errors.New("refusing publication acknowledgment for an unknown or stale observation")
	}
	if s.published[result.Name] == result.Generation {
		return nil
	}
	if err := s.write(publicationAck{Type: "published", Name: result.Name, Generation: result.Generation}); err != nil {
		return err
	}
	s.published[result.Name] = result.Generation
	return nil
}

// recorded returns the last durable observation for a resumed single pass.
// An expired positive observation must still be reverified before reporting.
func (s *verificationSession) recorded(name string) (verification, bool) {
	result, found := s.cache[name]
	return result, found
}

func (s *verificationSession) Close() error {
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

func verifyFile(ctx context.Context, inputPath, outputPath string, log io.Writer) error {
	return verifyFileWithRefresh(ctx, inputPath, outputPath, log, false)
}

func verifyFileWithRefresh(ctx context.Context, inputPath, outputPath string, log io.Writer, fresh bool) error {
	input, err := os.ReadFile(inputPath)
	if err != nil {
		return err
	}
	names, err := candidateNames(input)
	if err != nil {
		return err
	}
	// Snapshot input first: a growing discovery file is safe, but reusing it as
	// the JSONL destination would destroy its simple one-name-per-line format.
	inputInfo, err := os.Stat(inputPath)
	if err != nil {
		return err
	}
	if outputInfo, err := os.Stat(outputPath); err == nil && os.SameFile(inputInfo, outputInfo) {
		return fmt.Errorf("verification output must differ from the candidate input file")
	}
	session, err := openVerificationSession(outputPath)
	if err != nil {
		return err
	}
	defer session.Close()
	session.force = fresh
	available := 0
	for _, result := range session.cache {
		if result.Status == "available" {
			available++
		}
	}
	progressSource(ctx, len(names), len(session.cache))
	progressVerification(ctx, len(session.cache), available)
	gate, err := runtimeProviderGate("github-signup", session.verifier.interval)
	if err != nil {
		return err
	}
	session.verifier.gate = gate
	for _, name := range names {
		if !fresh {
			if previous, found := session.recorded(name); found && previous.Status == "available" && !session.needsPublish(name) {
				continue
			}
		}
		result, err := session.verify(ctx, name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(log, "%s: %s (checked %s)\n", name, result.Status, result.CheckedAt.UTC().Format(time.RFC3339))
		if result.Status == "available" {
			if err := publishResult(os.Stdout, name); err != nil {
				return err
			}
			if err := session.markPublished(result); err != nil {
				return err
			}
		}
	}
	return nil
}

// Accept either streamed candidate names or the durable discovery journal.
func candidateNames(input []byte) ([]string, error) {
	var order []string
	latest := make(map[string]string)
	for number, line := range strings.Split(string(input), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		status := "candidate"
		if strings.HasPrefix(name, "{") {
			var entry struct {
				Type   string `json:"type"`
				Name   string `json:"name"`
				Status string `json:"status"`
			}
			if json.Unmarshal([]byte(name), &entry) != nil {
				return nil, fmt.Errorf("invalid candidate JSONL at line %d", number+1)
			}
			if entry.Type == "config" {
				continue
			}
			if entry.Type != "lookup" || (entry.Status != "candidate" && entry.Status != "taken") {
				return nil, fmt.Errorf("unexpected candidate record at line %d", number+1)
			}
			name = entry.Name
			status = entry.Status
		}
		if !shortName.MatchString(name) {
			return nil, fmt.Errorf("invalid three- or four-character candidate at line %d", number+1)
		}
		if _, seen := latest[name]; !seen {
			order = append(order, name)
		}
		latest[name] = status
	}
	var names []string
	for _, name := range order {
		if latest[name] == "candidate" {
			names = append(names, name)
		}
	}
	return names, nil
}
