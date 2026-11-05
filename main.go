// Find username candidates, with an explicit opt-in GitHub rename mode.
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

const maxLookupBatch = 500

type config struct {
	Version            int         `json:"version"`
	Length             int         `json:"length"`
	Alphabet           string      `json:"alphabet"`
	Seed               int64       `json:"seed"`
	SourceHash         string      `json:"source_hash,omitempty"`
	Pattern            string      `json:"pattern,omitempty"`
	SourcePath         string      `json:"-"`
	Recheck            bool        `json:"-"`
	ResumeVerification bool        `json:"-"`
	Source             *nameSource `json:"-"`
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
	gate      *providerGate
	interval  time.Duration
	lastStart time.Time
	sleep     func(context.Context, time.Duration) error
	stopErr   error
}

func main() {
	platform := flag.String("platform", "github", "platform to check: github or discord")
	length := flag.Int("length", 3, "generated username length (GitHub: 3-4; Discord: 2-4)")
	digits := flag.Bool("digits", false, "include digits in addition to a-z")
	symbols := flag.Bool("symbols", false, "include _ and . in generated Discord names")
	delay := flag.Duration("delay", time.Second, "Discord pause between request starts (at least 1s)")
	n := flag.Int("n", 1000, "maximum names to check (normally new names; 0 = all remaining)")
	seed := flag.Int64("seed", 1, "deterministic shuffle seed; keep unchanged to resume")
	batch := flag.Int("batch", 100, "names per GraphQL request (1-500)")
	path := flag.String("out", "results.jsonl", "append-only JSONL results file; reused automatically to resume")
	verify := flag.String("verify", "", "verify candidates from this JSONL/text file using GitHub's public signup check; no API authentication needed")
	input := flag.String("input", "", "ordered wordlist of names, one per line; deduplicated and validated")
	pattern := flag.String("pattern", "", "name pattern with ? wildcards, for example a??")
	find := flag.Int("find", 0, "verify candidates as discovered and stop after this many available names")
	liveVerify := flag.Bool("live-verify", false, "verify all candidates while discovery continues")
	fresh := flag.Bool("fresh", false, "recheck saved observations, appending fresh results without truncating history")
	wait := flag.Bool("wait", false, "search only: wait through server cooldowns and resume saved progress")
	claim := flag.Bool("claim", false, "GitHub only: rename the specified account to the first verified available name, then stop")
	claimCookies := flag.String("claim-cookies", "", "cookie file for the explicitly enabled GitHub rename session")
	claimAccount := flag.String("claim-account", "", "expected current GitHub login; required with -claim")
	showVersion := flag.Bool("version", false, "print the version and exit")
	quiet := flag.Bool("quiet", false, "suppress progress output; errors remain visible")
	noColor := flag.Bool("no-color", false, "disable terminal colors (also respects NO_COLOR)")
	verifyOut := flag.String("verify-out", "verified.jsonl", "append-only JSONL verification results")
	statusFile := flag.String("status-file", "", "optional atomic JSON progress snapshot, updated once per second")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "hunt - find GitHub and Discord usernames.")
		fmt.Fprintln(os.Stderr, "Usage: hunt [flags]")
		fmt.Fprintln(os.Stderr, "Finder mode reports observations. GitHub account changes require explicit -claim settings.")
		fmt.Fprintln(os.Stderr)
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Fprintln(os.Stdout, "hunt", version)
		return
	}
	explicit := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if err := validatePlatformFlags(*platform, explicit); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if flag.NArg() != 0 || *n < 0 || *path == "" || *find < 0 {
		fmt.Fprintln(os.Stderr, "Use -n >= 0, -find >= 0, and a nonempty -out; see -help.")
		os.Exit(2)
	}
	if err := validateWaitFlags(*wait, *claim, *fresh); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	effectiveFind, err := validateClaimFlags(*platform, *claim, *claimCookies, *claimAccount, *find, explicit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	*find = effectiveFind
	if *platform == "github" && ((*length != 3 && *length != 4) || *batch < 1 || *batch > maxLookupBatch || *verifyOut == "") {
		fmt.Fprintln(os.Stderr, "Use -length 3 or 4, -n >= 0, -batch 1..500, and a nonempty -out; see -help.")
		os.Exit(2)
	}
	if *platform == "discord" && (*delay < time.Second || (*input == "" && (*length < 2 || *length > 4))) {
		fmt.Fprintln(os.Stderr, "Discord generation uses -length 2..4 and requires -delay >= 1s; -input accepts names of length 2..32.")
		os.Exit(2)
	}
	if *platform == "discord" && !explicit["out"] {
		*path = "discord-results.jsonl"
	}
	if err := validateStatusPaths(*statusFile, *claim, *input, *path, *verify, *verifyOut, *claimCookies); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, finishProgress, err := startSearchProgress(ctx, *statusFile, *platform)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Cannot create status snapshot:", err)
		os.Exit(1)
	}
	enableProviderGates()
	progress := terminalProgress(os.Stderr, stderrIsTerminal(os.Stderr), *quiet, *noColor)
	if *platform == "discord" {
		alphabet := "abcdefghijklmnopqrstuvwxyz"
		if *digits {
			alphabet += "0123456789"
		}
		if *symbols {
			alphabet += "_."
		}
		cfg := discordConfig{Version: 1, Platform: "discord", Length: *length, Alphabet: alphabet, Seed: *seed}
		cfg, err := configureDiscordSource(cfg, *input, *pattern)
		if err == nil {
			err = runSearchWithWait(ctx, *wait, progress, func() error {
				return runDiscord(ctx, discordOptions{Config: cfg, N: *n, Find: *find, Fresh: *fresh, Resume: *wait, Out: *path, Delay: *delay}, os.Stdout, progress)
			})
		}
		finishProgress(err)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Stopped:", err)
			os.Exit(1)
		}
		return
	}
	if *verify != "" {
		err := runSearchWithWait(ctx, *wait, progress, func() error {
			return verifyFileWithRefresh(ctx, *verify, *verifyOut, progress, *fresh)
		})
		finishProgress(err)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Stopped:", err)
			os.Exit(1)
		}
		return
	}
	alphabet := "abcdefghijklmnopqrstuvwxyz"
	if *digits {
		alphabet += "0123456789"
	}
	cfg := config{Version: 1, Length: *length, Alphabet: alphabet, Seed: *seed, Recheck: *fresh, ResumeVerification: *wait}
	cfg, err = configureSource(cfg, *input, *pattern)
	if err == nil {
		if *claim {
			var claimer *githubClaimer
			err = validateClaimPaths(*claimCookies, *path, *verifyOut, *input)
			if err == nil {
				claimer, err = newGitHubClaimer(*claimCookies, *claimAccount)
			}
			if err == nil {
				err = runPipelineWithClaim(ctx, cfg, *n, *batch, *path, *verifyOut, *find, os.Stdout, progress, claimer.Claim)
			}
		} else if *liveVerify || *find > 0 {
			err = runSearchWithWait(ctx, *wait, progress, func() error {
				return runPipeline(ctx, cfg, *n, *batch, *path, *verifyOut, *find, os.Stdout, progress)
			})
		} else {
			err = runSearchWithWait(ctx, *wait, progress, func() error {
				return run(ctx, cfg, *n, *batch, *path, os.Stdout, progress)
			})
		}
	}
	finishProgress(err)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Stopped:", err)
		os.Exit(1)
	}
}

func validateWaitFlags(wait, claim, fresh bool) error {
	if wait && claim {
		return errors.New("-wait is search-only and cannot be combined with -claim")
	}
	if wait && fresh {
		return errors.New("-wait resumes saved progress and cannot be combined with -fresh")
	}
	return nil
}

func validateClaimPaths(cookiePath string, dataPaths ...string) error {
	for _, path := range dataPaths {
		if sameFilePath(cookiePath, path) {
			return errors.New("the claim cookie file must differ from all input and results files")
		}
	}
	return nil
}

func validatePlatformFlags(platform string, explicit map[string]bool) error {
	switch platform {
	case "discord":
		for _, name := range []string{"verify", "verify-out", "batch", "live-verify", "claim", "claim-cookies", "claim-account"} {
			if explicit[name] {
				return fmt.Errorf("-%s is GitHub-only; Discord checks availability directly", name)
			}
		}
	case "github":
		for _, name := range []string{"symbols", "delay"} {
			if explicit[name] {
				return fmt.Errorf("-%s is Discord-only", name)
			}
		}
	default:
		return errors.New("-platform must be github or discord")
	}
	return nil
}

func validateClaimFlags(platform string, enabled bool, cookiePath, account string, find int, explicit map[string]bool) (int, error) {
	if !enabled {
		if cookiePath != "" || account != "" || explicit["claim-cookies"] || explicit["claim-account"] {
			return find, errors.New("-claim-cookies and -claim-account require explicit -claim")
		}
		return find, nil
	}
	if platform != "github" {
		return find, errors.New("-claim is supported only for GitHub")
	}
	if strings.TrimSpace(cookiePath) == "" || strings.TrimSpace(account) == "" {
		return find, errors.New("-claim requires both -claim-cookies and -claim-account")
	}
	if find < 0 {
		return find, errors.New("-find must not be negative")
	}
	if find > 1 || explicit["verify"] || explicit["live-verify"] {
		return find, errors.New("-claim handles one verified name; use -find 1 and omit -verify and -live-verify")
	}
	return 1, nil
}

func run(ctx context.Context, cfg config, n, batch int, path string, out, log io.Writer) error {
	return runDiscovery(ctx, cfg, n, batch, path, out, log, nil)
}

func runDiscovery(ctx context.Context, cfg config, n, batch int, path string, out, log io.Writer, onCandidate func(string) error) error {
	if sameFilePath(cfg.SourcePath, path) {
		return errors.New("the discovery results file must differ from the input wordlist")
	}
	names, err := githubSource(cfg)
	if err != nil {
		return err
	}
	journal, err := openStore(path, cfg)
	if err != nil {
		return err
	}
	defer journal.Close()
	progressSource(ctx, names.Len(), len(journal.done))
	if onCandidate != nil && !cfg.Recheck {
		for _, name := range journal.pendingCandidates() {
			if err := onCandidate(name); err != nil {
				return err
			}
		}
	}
	remaining := names.Len() - len(journal.done)
	if cfg.Recheck {
		remaining = names.Len()
		fmt.Fprintln(log, "Fresh mode: rechecking saved names; new observations append to history.")
	}
	if n == 0 || n > remaining {
		n = remaining
	}
	fmt.Fprintf(log, "Checking up to %d new names; %d previously saved, %d source names. Null lookups require manual verification.\n", n, len(journal.done), names.Len())
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
	gate, err := runtimeProviderGate("github-graphql", time.Second)
	if err != nil {
		return err
	}
	client := &githubClient{http: &http.Client{Timeout: 30 * time.Second}, token: token, interval: time.Second, gate: gate}
	checked, found, attempted := 0, 0, 0
	defer func() { fmt.Fprintf(log, "Saved %d new checks; %d candidates.\n", checked, found) }()
	for pos := 0; pos < names.Len() && attempted < n; {
		batchNames := make([]string, 0, batch)
		for pos < names.Len() && len(batchNames) < batch && len(batchNames) < n-attempted {
			name := names.At(pos)
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
		progressDiscovery(ctx, len(journal.done))
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
	cfg.ResumeVerification = false
	cfg.Source = nil
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
			s.record(entry.lookupResult)
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
		s.record(result)
	}
	return nil
}

// Only names that have ever been candidates need an ordered status record.
// Keeping taken observations in both maps and in the order slice multiplied
// replay memory without helping pending verification. Completion remains in
// done for every conclusive lookup; candidate transitions retain their latest
// state and enter FIFO order when they first become actionable.
func (s *store) record(result lookupResult) {
	_, tracked := s.latest[result.Name]
	if result.Status == "candidate" || tracked {
		if !tracked {
			s.order = append(s.order, result.Name)
		}
		s.latest[result.Name] = result
	}
	s.done[result.Name] = true
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
	if len(names) == 0 || len(names) > maxLookupBatch {
		return nil, errors.New("batch must contain 1-500 names")
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
		reply := g.ownerRequest(ctx, req, names)
		if reply.stateErr != nil {
			return nil, reply.stateErr
		}
		resp := reply.response
		if reply.transportErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt < 2 {
				continue
			}
			return nil, transientFailure("GitHub API connection attempts exhausted")
		}
		body, readErr := reply.body, reply.readErr
		limited := resp.StatusCode == http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0"
		if resp.StatusCode != http.StatusOK {
			if resp.StatusCode == http.StatusUnauthorized {
				return nil, errors.New("GitHub authentication failed (HTTP 401); check gh auth status or your token")
			}
			if searchChallengeResponse(body) {
				return nil, errors.New("GitHub API returned a challenge; this batch remains unchecked")
			}
			if limited {
				g.stopErr = cooldownFailure(graphQLCooldownUntil(resp.Header, ""), "GitHub API rate limit")
				return nil, g.stopErr
			}
			if resp.StatusCode == http.StatusForbidden {
				g.stopErr = errors.New("GitHub denied the API request (HTTP 403); check account access")
				return nil, g.stopErr
			}
			if resp.StatusCode >= 500 && resp.StatusCode <= 599 {
				if attempt < 2 {
					continue
				}
				return nil, transientFailure("GitHub API server attempts exhausted")
			}
			return nil, fmt.Errorf("GitHub returned HTTP %d; this batch remains unchecked", resp.StatusCode)
		}
		if readErr != nil {
			if limited {
				g.stopErr = cooldownFailure(graphQLCooldownUntil(resp.Header, ""), "GitHub API rate limit")
				return nil, g.stopErr
			}
			if attempt < 2 {
				continue
			}
			return nil, transientFailure("GitHub API response reads exhausted")
		}
		if len(body) > 4*1024*1024 {
			return nil, errors.New("unexpectedly large GitHub response; this batch remains unchecked")
		}
		results, parseErr := reply.results, reply.parseErr
		var cooldown *searchCooldownError
		if limited || errors.As(parseErr, &cooldown) {
			g.stopErr = parseErr
			if g.stopErr == nil {
				g.stopErr = cooldownFailure(graphQLCooldownUntil(resp.Header, ""), "GitHub API rate limit")
			}
		}
		return results, parseErr
	}
	return nil, errors.New("request attempts exhausted")
}

type ownerReply struct {
	response                                  *http.Response
	body                                      []byte
	results                                   []lookupResult
	transportErr, readErr, parseErr, stateErr error
}

// Hold a provider-wide lease across the request and response processing, then
// publish any server deadline before releasing it to another hunt process.
func (g *githubClient) ownerRequest(ctx context.Context, req *http.Request, names []string) (reply ownerReply) {
	var lease *providerLease
	if g.gate != nil {
		var err error
		lease, err = g.gate.Begin(ctx, g.lastStart, time.Time{})
		if err != nil {
			reply.stateErr = err
			return reply
		}
		defer func() { reply.stateErr = errors.Join(reply.stateErr, lease.Close()) }()
	}
	g.lastStart = time.Now()
	progressRequest(ctx, "discovery")
	reply.response, reply.transportErr = g.http.Do(req)
	if reply.transportErr != nil {
		return reply
	}
	reply.body, reply.readErr = io.ReadAll(io.LimitReader(reply.response.Body, 4*1024*1024+1))
	reply.response.Body.Close()
	if reply.response.StatusCode == http.StatusOK && reply.readErr == nil && len(reply.body) <= 4*1024*1024 {
		reply.results, reply.parseErr = parseResponse(reply.body, names, reply.response.Header)
	}
	if lease != nil {
		var cooldown *searchCooldownError
		until := time.Time{}
		if errors.As(reply.parseErr, &cooldown) {
			until = cooldown.Until
		}
		headers := reply.response.Header
		if reply.response.StatusCode == http.StatusTooManyRequests || headers.Get("Retry-After") != "" || headers.Get("X-RateLimit-Remaining") == "0" {
			if headerUntil := graphQLCooldownUntil(headers, ""); headerUntil.After(until) {
				until = headerUntil
			}
		}
		if !until.IsZero() {
			reply.stateErr = lease.Cooldown(until)
		}
	}
	return reply
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
		return nil, cooldownFailure(graphQLCooldownUntil(headers, rate.ResetAt), "GitHub API rate limit")
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
		return results, cooldownFailure(graphQLCooldownUntil(headers, rate.ResetAt), "GitHub API rate limit")
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

func graphQLCooldownUntil(headers http.Header, graphReset string) time.Time {
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
	if until.IsZero() {
		until = time.Now().Add(time.Minute)
	}
	return until
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
