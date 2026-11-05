// Find GitHub username candidates; this program never creates or changes accounts.
// A null lookup is only a candidate, not proof that GitHub permits registration.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"
)

var version = "dev"

type config struct {
	Version    int    `json:"version"`
	Length     int    `json:"length"`
	Alphabet   string `json:"alphabet"`
	Seed       int64  `json:"seed"`
	SourceHash string `json:"source_hash,omitempty"`
	Pattern    string `json:"pattern,omitempty"`
	SourcePath string `json:"-"`
	Recheck    bool   `json:"-"`
}

type lookupResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type store struct {
	file   *os.File
	cfg    config
	done   map[string]bool
	latest map[string]lookupResult
	order  []string
	unlock func() error
}

type githubClient struct {
	http      *http.Client
	token     string
	interval  time.Duration
	lastStart time.Time
	sleep     func(context.Context, time.Duration) error
	stopErr   error
}

func main() {
	length := flag.Int("length", 3, "username length: 3 or 4")
	digits := flag.Bool("digits", false, "include digits in addition to a-z")
	n := flag.Int("n", 1000, "maximum names to check (normally new names; 0 = all remaining)")
	seed := flag.Int64("seed", 1, "deterministic shuffle seed; keep unchanged to resume")
	batch := flag.Int("batch", 100, "names per GraphQL request (1-100)")
	path := flag.String("out", "results.jsonl", "append-only JSONL results file; reused automatically to resume")
	verify := flag.String("verify", "", "verify candidates from this JSONL/text file using GitHub's public signup check; no API authentication needed")
	input := flag.String("input", "", "ordered wordlist of names, one per line; deduplicated and validated")
	pattern := flag.String("pattern", "", "name pattern with ? wildcards, for example a??")
	find := flag.Int("find", 0, "verify candidates as discovered and stop after this many available names")
	liveVerify := flag.Bool("live-verify", false, "verify all candidates while discovery continues")
	fresh := flag.Bool("fresh", false, "recheck saved observations, appending fresh results without truncating history")
	showVersion := flag.Bool("version", false, "print the version and exit")
	quiet := flag.Bool("quiet", false, "suppress progress output; errors remain visible")
	noColor := flag.Bool("no-color", false, "disable terminal colors (also respects NO_COLOR)")
	verifyOut := flag.String("verify-out", "verified.jsonl", "append-only JSONL verification results")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "hunt - find three- and four-character GitHub username candidates.")
		fmt.Fprintln(os.Stderr, "Usage: hunt [flags]")
		fmt.Fprintln(os.Stderr, "Results are observations to verify; no account changes are submitted.")
		fmt.Fprintln(os.Stderr)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Fprintln(os.Stdout, "hunt", version)
		return
	}
	if flag.NArg() != 0 || (*length != 3 && *length != 4) || *n < 0 || *batch < 1 || *batch > 100 || *path == "" || *verifyOut == "" || *find < 0 {
		fmt.Fprintln(os.Stderr, "Use -length 3 or 4, -n >= 0, -batch 1..100, and a nonempty -out; see -help.")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	progress := terminalProgress(os.Stderr, stderrIsTerminal(os.Stderr), *quiet, *noColor)
	if *verify != "" {
		if err := verifyFileWithRefresh(ctx, *verify, *verifyOut, progress, *fresh); err != nil {
			fmt.Fprintln(os.Stderr, "Stopped:", err)
			os.Exit(1)
		}
		return
	}
	alphabet := "abcdefghijklmnopqrstuvwxyz"
	if *digits {
		alphabet += "0123456789"
	}
	cfg := config{Version: 1, Length: *length, Alphabet: alphabet, Seed: *seed, Recheck: *fresh}
	cfg, err := configureSource(cfg, *input, *pattern)
	if err == nil {
		if *liveVerify || *find > 0 {
			err = runPipeline(ctx, cfg, *n, *batch, *path, *verifyOut, *find, os.Stdout, progress)
		} else {
			err = run(ctx, cfg, *n, *batch, *path, os.Stdout, progress)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Stopped:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config, n, batch int, path string, out, log io.Writer) error {
	return runDiscovery(ctx, cfg, n, batch, path, out, log, nil)
}

func runDiscovery(ctx context.Context, cfg config, n, batch int, path string, out, log io.Writer, onCandidate func(string) error) error {
	if sameFilePath(cfg.SourcePath, path) {
		return errors.New("the discovery results file must differ from the input wordlist")
	}
	names, err := discoveryNames(cfg)
	if err != nil {
		return err
	}
	journal, err := openStore(path, cfg)
	if err != nil {
		return err
	}
	defer journal.Close()
	if onCandidate != nil && !cfg.Recheck {
		for _, name := range journal.pendingCandidates() {
			if err := onCandidate(name); err != nil {
				return err
			}
		}
	}
	remaining := len(names) - len(journal.done)
	if cfg.Recheck {
		remaining = len(names)
		fmt.Fprintln(log, "Fresh mode: rechecking saved names; new observations append to history.")
	}
	if n == 0 || n > remaining {
		n = remaining
	}
	fmt.Fprintf(log, "Checking up to %d new names; %d previously saved, %d source names. Null lookups require manual verification.\n", n, len(journal.done), len(names))
	defer func() {
		if onCandidate != nil {
			fmt.Fprintln(log, "Resume verification by repeating your original -find or -live-verify command with the same journals.")
			return
		}
		digitFlag, sourceFlags := "", ""
		if strings.Contains(cfg.Alphabet, "0") {
			digitFlag = " -digits"
		}
		if cfg.SourcePath != "" {
			sourceFlags = " -input " + shellQuote(cfg.SourcePath)
		}
		if cfg.Pattern != "" {
			sourceFlags += " -pattern " + shellQuote(cfg.Pattern)
		}
		fmt.Fprintf(log, "Resume with the same file: go run . -length %d%s -seed %d -n %d -batch %d -out %s%s\n", cfg.Length, digitFlag, cfg.Seed, n, batch, shellQuote(path), sourceFlags)
	}()
	if n == 0 {
		return nil
	}
	token, err := githubToken(ctx)
	if err != nil {
		return err
	}
	client := &githubClient{http: &http.Client{Timeout: 30 * time.Second}, token: token, interval: time.Second}
	checked, found, attempted := 0, 0, 0
	defer func() { fmt.Fprintf(log, "Saved %d new checks; %d candidates.\n", checked, found) }()
	for pos := 0; pos < len(names) && attempted < n; {
		batchNames := make([]string, 0, batch)
		for pos < len(names) && len(batchNames) < batch && len(batchNames) < n-attempted {
			name := names[pos]
			pos++
			if cfg.Recheck || !journal.done[name] {
				batchNames = append(batchNames, name)
			}
		}
		if len(batchNames) == 0 {
			break
		}
		attempted += len(batchNames)
		results, lookupErr := client.lookupResolved(ctx, batchNames, log)
		if err := journal.appendBatch(results); err != nil {
			return err
		}
		checked += len(results)
		for _, result := range results {
			if result.Status == "candidate" {
				found++
				if onCandidate != nil {
					if err := onCandidate(result.Name); err != nil {
						return err
					}
				} else if _, err := fmt.Fprintln(out, result.Name); err != nil {
					return err
				}
			}
		}
		fmt.Fprintf(log, "%d/%d attempted names saved; %d candidates.\n", checked, attempted, found)
		if lookupErr != nil {
			return lookupErr
		}
	}
	return nil
}

func githubToken(ctx context.Context) (string, error) {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token, nil
		}
	}
	authCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Capture both streams: never echo credentials or diagnostic output from gh.
	cmd := exec.CommandContext(authCtx, "gh", "auth", "token", "--hostname", "github.com")
	data, err := cmd.Output()
	if err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data)), nil
	}
	return "", errors.New("GitHub authentication required: run `gh auth login --hostname github.com`, or set GH_TOKEN/GITHUB_TOKEN")
}

func namespaceSize(cfg config) int {
	total := 1
	for i := 0; i < cfg.Length; i++ {
		total *= len(cfg.Alphabet)
	}
	return total
}

func nameAt(value int, cfg config) string {
	name := make([]byte, cfg.Length)
	for i := len(name) - 1; i >= 0; i-- {
		name[i] = cfg.Alphabet[value%len(cfg.Alphabet)]
		value /= len(cfg.Alphabet)
	}
	return string(name)
}

func (s *store) Close() error {
	err := s.file.Close()
	if s.unlock != nil {
		err = errors.Join(err, s.unlock())
		s.unlock = nil
	}
	return err
}

func (s *store) pendingCandidates() []string {
	var names []string
	for _, name := range s.order {
		if s.latest[name].Status == "candidate" {
			names = append(names, name)
		}
	}
	return names
}

func persistentConfig(cfg config) config {
	cfg.SourcePath, cfg.Recheck = "", false
	return cfg
}

func openStore(path string, cfg config) (*store, error) {
	unlock, err := lockJournal(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		unlock()
		return nil, err
	}
	s := &store{file: file, cfg: cfg, done: make(map[string]bool), latest: make(map[string]lookupResult), unlock: unlock}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	reader := bufio.NewReader(file)
	var offset int64
	line := 0
	for {
		data, readErr := reader.ReadBytes('\n')
		if len(data) == 0 && readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		var entry struct {
			Type string `json:"type"`
			config
			lookupResult
			CheckedAt string `json:"checked_at"`
		}
		if err := json.Unmarshal(data, &entry); err != nil {
			if readErr == io.EOF {
				if line == 0 {
					return nil, errors.New("invalid journal header; refusing to truncate an unrecognized file")
				}
				// A crash may leave one incomplete final write; earlier records remain.
				if err := file.Truncate(offset); err != nil {
					return nil, err
				}
				break
			}
			return nil, fmt.Errorf("invalid JSONL at line %d; use an intact file or a new -out", line+1)
		}
		if line == 0 {
			if entry.Type != "config" || entry.config != persistentConfig(cfg) {
				return nil, errors.New("results file configuration differs (version/length/alphabet/seed); reuse its flags or choose a new -out")
			}
		} else {
			if entry.Type != "lookup" || !validResult(entry.lookupResult, cfg) {
				return nil, fmt.Errorf("invalid lookup record at line %d", line+1)
			}
			if _, err := time.Parse(time.RFC3339Nano, entry.CheckedAt); err != nil {
				return nil, fmt.Errorf("invalid timestamp at line %d", line+1)
			}
			if !s.done[entry.Name] {
				s.order = append(s.order, entry.Name)
			}
			s.done[entry.Name] = true
			s.latest[entry.Name] = entry.lookupResult
		}
		line++
		offset += int64(len(data))
		if readErr == io.EOF {
			// A complete last record without a newline is safe to retain.
			if _, err := file.Seek(0, io.SeekEnd); err != nil {
				return nil, err
			}
			if _, err := file.WriteString("\n"); err != nil {
				return nil, err
			}
			break
		}
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	if line == 0 {
		if err := s.write(struct {
			Type string `json:"type"`
			config
		}{"config", cfg}); err != nil {
			return nil, err
		}
	} else if err := file.Sync(); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

func validResult(result lookupResult, cfg config) bool {
	if len(result.Name) != cfg.Length || (result.Status != "taken" && result.Status != "candidate") {
		return false
	}
	for _, c := range result.Name {
		if !strings.ContainsRune(cfg.Alphabet, c) {
			return false
		}
	}
	return true
}

func (s *store) append(result lookupResult) error { return s.appendBatch([]lookupResult{result}) }

func (s *store) appendBatch(results []lookupResult) error {
	var data bytes.Buffer
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, result := range results {
		if !validResult(result, s.cfg) {
			return errors.New("refusing to save an invalid or inconclusive lookup")
		}
		entry := struct {
			Type string `json:"type"`
			lookupResult
			CheckedAt string `json:"checked_at"`
		}{"lookup", result, stamp}
		if err := json.NewEncoder(&data).Encode(entry); err != nil {
			return err
		}
	}
	if data.Len() == 0 {
		return nil
	}
	written, err := s.file.Write(data.Bytes())
	if err != nil {
		return err
	}
	if written != data.Len() {
		return io.ErrShortWrite
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	for _, result := range results {
		if !s.done[result.Name] {
			s.order = append(s.order, result.Name)
		}
		s.done[result.Name] = true
		s.latest[result.Name] = result
	}
	return nil
}

func (s *store) write(value any) error {
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

func (g *githubClient) pause(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	if g.sleep != nil {
		return g.sleep(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (g *githubClient) lookup(ctx context.Context, names []string) ([]lookupResult, error) {
	if g.stopErr != nil {
		return nil, g.stopErr
	}
	if len(names) == 0 || len(names) > 100 {
		return nil, errors.New("batch must contain 1-100 names")
	}
	var query strings.Builder
	query.WriteString("query {")
	for i, name := range names {
		fmt.Fprintf(&query, "u%d:repositoryOwner(login:%s){login} ", i, strconv.Quote(name))
	}
	query.WriteString("rateLimit{cost remaining resetAt}}")
	requestBody, _ := json.Marshal(map[string]string{"query": query.String()})
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt > 0 {
			if err := g.pause(ctx, time.Second*time.Duration(1<<(attempt-1))); err != nil {
				return nil, err
			}
		}
		if err := g.pause(ctx, g.interval-time.Since(g.lastStart)); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", bytes.NewReader(requestBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+g.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "github-username-finder")
		g.lastStart = time.Now()
		resp, err := g.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt < 2 {
				continue
			}
			return nil, errors.New("GitHub connection failed after 3 attempts; no names in this batch were marked checked")
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
		resp.Body.Close()
		limited := resp.StatusCode == http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0"
		if resp.StatusCode != http.StatusOK {
			if limited || resp.StatusCode == http.StatusForbidden {
				g.stopErr = fmt.Errorf("GitHub denied or rate limited the request (HTTP %d); %s", resp.StatusCode, retryHint(resp.Header, ""))
				return nil, g.stopErr
			}
			if resp.StatusCode >= 500 && resp.StatusCode <= 599 && attempt < 2 {
				continue
			}
			if resp.StatusCode == http.StatusUnauthorized {
				return nil, errors.New("GitHub authentication failed (HTTP 401); check gh auth status or your token")
			}
			return nil, fmt.Errorf("GitHub returned HTTP %d; this batch remains unchecked", resp.StatusCode)
		}
		if readErr != nil {
			if limited {
				g.stopErr = fmt.Errorf("rate limit reached; %s", retryHint(resp.Header, ""))
				return nil, g.stopErr
			}
			if attempt < 2 {
				continue
			}
			return nil, errors.New("could not read GitHub response after 3 attempts; this batch remains unchecked")
		}
		if len(body) > 4*1024*1024 {
			return nil, errors.New("unexpectedly large GitHub response; this batch remains unchecked")
		}
		results, parseErr := parseResponse(body, names, resp.Header)
		if limited || (parseErr != nil && strings.Contains(parseErr.Error(), "rate limit")) {
			g.stopErr = parseErr
			if g.stopErr == nil {
				g.stopErr = fmt.Errorf("rate limit reached; %s", retryHint(resp.Header, ""))
			}
		}
		return results, parseErr
	}
	return nil, errors.New("request attempts exhausted")
}

func parseResponse(body []byte, names []string, headers http.Header) ([]lookupResult, error) {
	var response struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Type       string `json:"type"`
			Path       []any  `json:"path"`
			Extensions struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, errors.New("invalid GraphQL response; this batch remains unchecked")
	}
	blocked := make(map[string]bool)
	globalError, rateError := false, false
	for _, item := range response.Errors {
		for _, code := range []string{item.Type, item.Extensions.Code, item.Extensions.Type} {
			if code == "FORBIDDEN" || code == "UNAUTHENTICATED" || code == "UNAUTHORIZED" || code == "GRAPHQL_VALIDATION_FAILED" {
				globalError = true
			}
		}
		if item.Type == "RATE_LIMITED" || item.Extensions.Code == "RATE_LIMITED" || item.Extensions.Type == "RATE_LIMITED" || strings.Contains(strings.ToLower(item.Message), "rate limit") {
			rateError = true
		}
		if len(item.Path) == 0 {
			globalError = true
		} else if alias, ok := item.Path[0].(string); ok {
			known := false
			for i := range names {
				if alias == fmt.Sprintf("u%d", i) {
					known = true
					break
				}
			}
			if known {
				blocked[alias] = true
			} else {
				globalError = true
			}
		} else {
			globalError = true
		}
	}
	var rate struct {
		Remaining *int   `json:"remaining"`
		ResetAt   string `json:"resetAt"`
	}
	json.Unmarshal(response.Data["rateLimit"], &rate)
	if rateError {
		return nil, fmt.Errorf("GraphQL rate limit reached; %s", retryHint(headers, rate.ResetAt))
	}
	if response.Data == nil {
		return nil, errors.New("invalid GraphQL response; this batch remains unchecked")
	}
	if globalError {
		return nil, errors.New("GraphQL returned a global error; this batch remains unchecked")
	}
	results := make([]lookupResult, 0, len(names))
	var unknown []string
	for i, name := range names {
		alias := fmt.Sprintf("u%d", i)
		raw, present := response.Data[alias]
		if !present || blocked[alias] {
			unknown = append(unknown, name)
			continue
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			results = append(results, lookupResult{Name: name, Status: "candidate"})
			continue
		}
		var owner struct {
			Login string `json:"login"`
		}
		if json.Unmarshal(raw, &owner) != nil || owner.Login == "" || !strings.EqualFold(owner.Login, name) {
			return nil, errors.New("unexpected GraphQL owner schema; this batch remains unchecked")
		}
		results = append(results, lookupResult{Name: name, Status: "taken"})
	}
	if headers.Get("X-RateLimit-Remaining") == "0" || headers.Get("Retry-After") != "" || (rate.Remaining != nil && *rate.Remaining == 0) {
		return results, fmt.Errorf("rate limit reached; %s", retryHint(headers, rate.ResetAt))
	}
	if len(unknown) > 0 {
		return results, &partialLookupError{Names: unknown}
	}
	return results, nil
}

func retryHint(headers http.Header, graphReset string) string {
	var until time.Time
	if value := headers.Get("Retry-After"); value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds >= 0 {
			until = time.Now().Add(time.Duration(seconds) * time.Second)
		} else if parsed, err := http.ParseTime(value); err == nil {
			until = parsed
		}
	}
	if headers.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if parsed := time.Unix(reset, 0); parsed.After(until) {
				until = parsed
			}
		}
	}
	if parsed, err := time.Parse(time.RFC3339, graphReset); err == nil && parsed.After(until) {
		until = parsed
	}
	if !until.IsZero() {
		return "do not resume before " + until.UTC().Format(time.RFC3339)
	}
	return "wait at least one minute before retrying; if still denied, check access or wait longer"
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
