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
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checkedAt"`
	Source    string    `json:"source"`
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
}

type verificationSession struct {
	file     *os.File
	unlock   func() error
	verifier *webVerifier
	cache    map[string]verification
	force    bool
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
	if v.clock().Before(v.cooldown) {
		return "", fmt.Errorf("web verification cooldown; do not retry before %s", v.cooldown.UTC().Format(time.RFC3339))
	}
	if v.sleep == nil {
		v.sleep = verificationPause
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !v.last.IsZero() {
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
			return "", err
		}
		req.Header.Set("Accept", "text/fragment+html")
		req.Header.Set("User-Agent", "github-finder/2.0")
		v.last = v.clock()
		if v.beforeRequest != nil {
			if err := v.beforeRequest(v.last); err != nil {
				return "", fmt.Errorf("could not persist verification request time: %w", err)
			}
		}
		response, err := v.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if attempt < 2 {
				continue
			}
			return "", errors.New("verification connection failed after 3 attempts; availability unknown")
		}
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusForbidden || response.Header.Get("Retry-After") != "" {
			response.Body.Close()
			v.cooldown = verificationCooldown(response.Header.Get("Retry-After"), v.clock())
			if v.onCooldown != nil {
				if err := v.onCooldown(v.cooldown); err != nil {
					return "", fmt.Errorf("web verification stopped (HTTP %d); do not retry before %s; could not persist cooldown: %w", response.StatusCode, v.cooldown.UTC().Format(time.RFC3339), err)
				}
			}
			return "", fmt.Errorf("web verification stopped (HTTP %d); do not retry before %s; no more requests sent", response.StatusCode, v.cooldown.UTC().Format(time.RFC3339))
		}
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusUnprocessableEntity && response.StatusCode < 500 {
			response.Body.Close()
			return "", fmt.Errorf("unrecognized verification response (HTTP %d); availability unknown", response.StatusCode)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 128*1024+1))
		response.Body.Close()
		if response.StatusCode >= 500 && response.StatusCode <= 599 {
			if attempt < 2 {
				continue
			}
			return "", fmt.Errorf("verification HTTP %d after 3 attempts; availability unknown", response.StatusCode)
		}
		if readErr != nil {
			if attempt < 2 {
				continue
			}
			return "", errors.New("could not read verification response after 3 attempts; availability unknown")
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
		if response.StatusCode == http.StatusUnprocessableEntity && strings.Contains(text, "Username "+name+" is not available.") {
			return "unavailable", nil
		}
		return "", fmt.Errorf("unrecognized verification response (HTTP %d); availability unknown; endpoint may have changed", response.StatusCode)
	}
	return "", errors.New("verification attempts exhausted; availability unknown")
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
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		unlock()
		return nil, err
	}
	s := &verificationSession{file: file, unlock: unlock, cache: make(map[string]verification), verifier: &webVerifier{client: &http.Client{Timeout: 15 * time.Second}, interval: 7 * time.Second}}
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
		var saved verification
		if control.Type != "" || json.Unmarshal(line, &saved) != nil || !shortName.MatchString(saved.Name) || (saved.Status != "available" && saved.Status != "unavailable") || saved.CheckedAt.IsZero() || saved.Source != validationURL {
			return fmt.Errorf("invalid verification journal at line %d", lineNo)
		}
		if previous, ok := s.cache[saved.Name]; !ok || saved.CheckedAt.After(previous.CheckedAt) {
			s.cache[saved.Name] = saved
		}
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
	result := verification{Name: name, Status: status, CheckedAt: s.verifier.clock().UTC(), Source: validationURL}
	if err := s.write(result); err != nil {
		return verification{}, err
	}
	s.cache[name] = result
	return result, nil
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
	for _, name := range names {
		result, err := session.verify(ctx, name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(log, "%s: %s (checked %s)\n", name, result.Status, result.CheckedAt.UTC().Format(time.RFC3339))
		if result.Status == "available" {
			if _, err := fmt.Fprintln(os.Stdout, name); err != nil {
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
