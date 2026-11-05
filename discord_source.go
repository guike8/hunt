package main

import (
	"bufio"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
)

type discordConfig struct {
	Version    int    `json:"version"`
	Platform   string `json:"platform"`
	Length     int    `json:"length"`
	Alphabet   string `json:"alphabet"`
	Pattern    string `json:"pattern,omitempty"`
	SourceHash string `json:"source_hash,omitempty"`
	Seed       int64  `json:"seed"`
	SourcePath string `json:"-"`
}

func persistentDiscordConfig(cfg discordConfig) discordConfig {
	cfg.SourcePath = ""
	return cfg
}

func configureDiscordSource(cfg discordConfig, input, pattern string) (discordConfig, error) {
	if input != "" && pattern != "" {
		return cfg, errors.New("choose either -input or -pattern")
	}
	cfg.SourcePath, cfg.Pattern, cfg.SourceHash = input, pattern, ""
	if input != "" {
		// Wordlists may mix all supported Discord lengths; -length controls
		// generated sources only, so it does not bind an input file's identity.
		cfg.Length = 0
		names, err := readDiscordWordlist(input)
		if err != nil {
			return cfg, err
		}
		cfg.SourceHash = wordlistHash(names)
	}
	return cfg, validateDiscordSource(cfg)
}

func validateDiscordSource(cfg discordConfig) error {
	if cfg.Platform != "discord" || cfg.Version != 1 {
		return errors.New("invalid Discord source configuration")
	}
	if cfg.SourcePath != "" || cfg.SourceHash != "" {
		if cfg.SourcePath == "" || cfg.SourceHash == "" || cfg.Pattern != "" || cfg.Length != 0 {
			return errors.New("Discord wordlist requires its original -input file")
		}
		return nil
	}
	if cfg.Length < 2 || cfg.Length > 4 {
		return errors.New("Discord generation requires -length 2, 3, or 4")
	}
	if cfg.Alphabet == "" || len(cfg.Alphabet) > 38 {
		return errors.New("invalid Discord generation alphabet")
	}
	seen := make(map[rune]bool)
	for _, c := range cfg.Alphabet {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789_.", c) || seen[c] {
			return errors.New("Discord alphabet must contain unique lowercase letters, digits, underscores, or periods")
		}
		seen[c] = true
	}
	if cfg.Pattern != "" {
		if len(cfg.Pattern) != cfg.Length {
			return fmt.Errorf("Discord -pattern must contain %d characters", cfg.Length)
		}
		for _, c := range cfg.Pattern {
			if c != '?' && !strings.ContainsRune(cfg.Alphabet, c) {
				return errors.New("Discord -pattern must contain only ? or characters from the selected alphabet")
			}
		}
		if strings.Contains(cfg.Pattern, "..") {
			return errors.New("Discord names cannot contain consecutive periods")
		}
	}
	return nil
}

func readDiscordWordlist(path string) ([]string, error) {
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
		if !validDiscordName(name) {
			return nil, fmt.Errorf("Discord wordlist line %d: expected a valid 2-32 character name (a-z, 0-9, _, .; no consecutive periods)", line)
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
		return nil, errors.New("Discord wordlist contains no names")
	}
	return names, nil
}

func discordNames(cfg discordConfig) ([]string, error) {
	if err := validateDiscordSource(cfg); err != nil {
		return nil, err
	}
	if cfg.SourcePath != "" {
		names, err := readDiscordWordlist(cfg.SourcePath)
		if err != nil {
			return nil, err
		}
		if wordlistHash(names) != cfg.SourceHash {
			return nil, errors.New("Discord wordlist changed; reuse its original contents or choose a new -out")
		}
		return names, nil
	}
	pattern := cfg.Pattern
	if pattern == "" {
		pattern = strings.Repeat("?", cfg.Length)
	}
	var positions []int
	total := 1
	for i := range pattern {
		if pattern[i] == '?' {
			positions = append(positions, i)
			total *= len(cfg.Alphabet)
		}
	}
	names := make([]string, 0, total)
	for value := 0; value < total; value++ {
		name, index := []byte(pattern), value
		for i := len(positions) - 1; i >= 0; i-- {
			name[positions[i]] = cfg.Alphabet[index%len(cfg.Alphabet)]
			index /= len(cfg.Alphabet)
		}
		if validDiscordName(string(name)) {
			names = append(names, string(name))
		}
	}
	if len(names) == 0 {
		return nil, errors.New("Discord source generates no valid names")
	}
	rand.New(rand.NewSource(cfg.Seed)).Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	return names, nil
}
