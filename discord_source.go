package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

type discordConfig struct {
	Version    int         `json:"version"`
	Platform   string      `json:"platform"`
	Length     int         `json:"length"`
	Alphabet   string      `json:"alphabet"`
	Pattern    string      `json:"pattern,omitempty"`
	SourceHash string      `json:"source_hash,omitempty"`
	Seed       int64       `json:"seed"`
	SourcePath string      `json:"-"`
	Source     *nameSource `json:"-"`
}

func persistentDiscordConfig(cfg discordConfig) discordConfig {
	cfg.SourcePath = ""
	cfg.Source = nil
	return cfg
}

func configureDiscordSource(cfg discordConfig, input, pattern string) (discordConfig, error) {
	if input != "" && pattern != "" {
		return cfg, errors.New("choose either -input or -pattern")
	}
	cfg.SourcePath, cfg.Pattern, cfg.SourceHash = input, pattern, ""
	cfg.Source = nil
	if input != "" {
		// Wordlists may mix all supported Discord lengths; -length controls
		// generated sources only, so it does not bind an input file's identity.
		cfg.Length = 0
		names, err := readDiscordWordlist(input)
		if err != nil {
			return cfg, err
		}
		cfg.SourceHash = wordlistHash(names)
		cfg.Source = &nameSource{key: sourceKey{kind: "discord", alphabet: cfg.Alphabet, pattern: cfg.Pattern, hash: cfg.SourceHash, path: cfg.SourcePath, length: cfg.Length, seed: cfg.Seed}, words: names}
	}
	if err := validateDiscordSource(cfg); err != nil {
		return cfg, err
	}
	if cfg.Source == nil {
		source, err := discordSource(cfg)
		if err != nil {
			return cfg, err
		}
		cfg.Source = source
	}
	return cfg, nil
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
	source, err := discordSource(cfg)
	if err != nil {
		return nil, err
	}
	return source.materialize(), nil
}
