package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func configureSource(cfg config, inputPath, pattern string) (config, error) {
	if inputPath != "" && pattern != "" {
		return cfg, errors.New("choose either -input or -pattern")
	}
	cfg.SourcePath, cfg.Pattern = inputPath, pattern
	cfg.SourceHash = ""
	cfg.Source = nil
	if err := validateSourceConfig(cfg); err != nil {
		return cfg, err
	}
	if inputPath != "" {
		names, err := readWordlist(inputPath, cfg)
		if err != nil {
			return cfg, err
		}
		cfg.SourceHash = wordlistHash(names)
		cfg.Source = &nameSource{key: sourceKey{kind: "github", alphabet: cfg.Alphabet, pattern: cfg.Pattern, hash: cfg.SourceHash, path: cfg.SourcePath, length: cfg.Length, seed: cfg.Seed}, words: names}
	}
	if cfg.Source == nil {
		source, err := githubSource(cfg)
		if err != nil {
			return cfg, err
		}
		cfg.Source = source
	}
	return cfg, nil
}

// Validate source settings without materializing the search space. Wordlists
// are read and hashed separately, since their order is part of source identity.
func validateSourceConfig(cfg config) error {
	if cfg.Length != 3 && cfg.Length != 4 {
		return errors.New("source length must be 3 or 4")
	}
	if cfg.Alphabet == "" {
		return errors.New("source alphabet must not be empty")
	}
	seen := make(map[rune]bool)
	for _, c := range cfg.Alphabet {
		if ((c < 'a' || c > 'z') && (c < '0' || c > '9')) || seen[c] {
			return errors.New("source alphabet must contain unique lowercase letters or digits")
		}
		seen[c] = true
	}
	if cfg.Pattern != "" {
		if cfg.SourcePath != "" || cfg.SourceHash != "" {
			return errors.New("choose either -input or -pattern")
		}
		if len(cfg.Pattern) != cfg.Length {
			return fmt.Errorf("-pattern must have %d characters", cfg.Length)
		}
		for _, c := range cfg.Pattern {
			if c != '?' && !strings.ContainsRune(cfg.Alphabet, c) {
				return errors.New("-pattern must contain only ? or characters from the selected alphabet")
			}
		}
	}
	return nil
}

func wordlistHash(names []string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(names, "\n")+"\n")))
}

func readWordlist(path string, cfg config) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var names []string
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		name := strings.ToLower(strings.TrimSpace(scanner.Text()))
		if name == "" {
			continue
		}
		if !validResult(lookupResult{Name: name, Status: "candidate"}, cfg) {
			return nil, fmt.Errorf("wordlist line %d: expected %d characters from %s", line, cfg.Length, cfg.Alphabet)
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("wordlist contains no names")
	}
	return names, nil
}

// discoveryNames preserves wordlist order and shuffles generated sources using
// the same seeded permutation as version 1, keeping existing journals resumable.
func discoveryNames(cfg config) ([]string, error) {
	source, err := githubSource(cfg)
	if err != nil {
		return nil, err
	}
	return source.materialize(), nil
}

type partialLookupError struct{ Names []string }

func (e *partialLookupError) Error() string {
	return fmt.Sprintf("%d GraphQL lookups were inconclusive", len(e.Names))
}

// Retry unresolved aliases once in smaller batches. Unknown names remain absent
// from the journal, so a later run can retry them without losing other progress.
func (g *githubClient) lookupResolved(ctx context.Context, names []string, log io.Writer) ([]lookupResult, error) {
	results, err := g.lookup(ctx, names)
	var partial *partialLookupError
	if !errors.As(err, &partial) {
		return results, err
	}
	size := (len(partial.Names) + 1) / 2
	for start := 0; start < len(partial.Names); start += size {
		end := start + size
		if end > len(partial.Names) {
			end = len(partial.Names)
		}
		retry, retryErr := g.lookup(ctx, partial.Names[start:end])
		results = append(results, retry...)
		var unresolved *partialLookupError
		if errors.As(retryErr, &unresolved) {
			fmt.Fprintf(log, "Still unknown after one retry (left unchecked): %s\n", strings.Join(unresolved.Names, ", "))
			continue
		}
		if retryErr != nil {
			return results, retryErr
		}
	}
	return results, nil
}
