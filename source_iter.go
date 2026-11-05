package main

import (
	"errors"
	"math/rand"
	"strings"
)

// A source holds only shuffled integer positions for generated names. Strings
// are created when a batch needs them, instead of materializing the namespace.
// Sources are immutable after construction and may be shared by retry passes.
type nameSource struct {
	key       sourceKey
	words     []string
	order     []int
	positions []int
}

type sourceKey struct {
	kind, alphabet, pattern, hash, path string
	length                              int
	seed                                int64
}

func (s *nameSource) Len() int {
	if s.words != nil {
		return len(s.words)
	}
	return len(s.order)
}

func (s *nameSource) At(i int) string {
	if s.words != nil {
		return s.words[i]
	}
	var name [4]byte
	if s.key.pattern == "" {
		value := s.order[i]
		for j := s.key.length - 1; j >= 0; j-- {
			name[j] = s.key.alphabet[value%len(s.key.alphabet)]
			value /= len(s.key.alphabet)
		}
	} else {
		copy(name[:], s.key.pattern)
		value := s.order[i]
		for j := len(s.positions) - 1; j >= 0; j-- {
			name[s.positions[j]] = s.key.alphabet[value%len(s.key.alphabet)]
			value /= len(s.key.alphabet)
		}
	}
	return string(name[:s.key.length])
}

func (s *nameSource) materialize() []string {
	names := make([]string, s.Len())
	for i := range names {
		names[i] = s.At(i)
	}
	return names
}

func newGeneratedSource(key sourceKey) (*nameSource, error) {
	s := &nameSource{key: key}
	total := 1
	pattern := key.pattern
	if pattern == "" {
		pattern = strings.Repeat("?", key.length)
	}
	for i := range pattern {
		if pattern[i] == '?' {
			s.positions = append(s.positions, i)
			total *= len(key.alphabet)
		}
	}
	if key.kind == "github" {
		s.order = rand.New(rand.NewSource(key.seed)).Perm(total)
		return s, nil
	}
	// Preserve Discord's original filtered-then-Shuffle order exactly. Filtering
	// integer positions avoids retaining all valid generated strings.
	s.order = make([]int, 0, total)
	for i := 0; i < total; i++ {
		var name [4]byte
		copy(name[:], pattern)
		value := i
		for j := len(s.positions) - 1; j >= 0; j-- {
			name[s.positions[j]] = key.alphabet[value%len(key.alphabet)]
			value /= len(key.alphabet)
		}
		valid := true
		for j := 1; j < key.length; j++ {
			if name[j-1] == '.' && name[j] == '.' {
				valid = false
				break
			}
		}
		if valid {
			s.order = append(s.order, i)
		}
	}
	if len(s.order) == 0 {
		return nil, errors.New("Discord source generates no valid names")
	}
	rand.New(rand.NewSource(key.seed)).Shuffle(len(s.order), func(i, j int) { s.order[i], s.order[j] = s.order[j], s.order[i] })
	return s, nil
}

func githubSource(cfg config) (*nameSource, error) {
	if err := validateSourceConfig(cfg); err != nil {
		return nil, err
	}
	key := sourceKey{kind: "github", alphabet: cfg.Alphabet, pattern: cfg.Pattern, hash: cfg.SourceHash, path: cfg.SourcePath, length: cfg.Length, seed: cfg.Seed}
	if cfg.SourceHash != "" || cfg.SourcePath != "" {
		if cfg.SourcePath == "" {
			return nil, errors.New("this journal requires its original -input wordlist")
		}
		words, err := readWordlist(cfg.SourcePath, cfg)
		if err != nil {
			return nil, err
		}
		if wordlistHash(words) != cfg.SourceHash {
			return nil, errors.New("wordlist contents changed; reuse the original list or choose a new -out")
		}
		if cfg.Source != nil && cfg.Source.key == key {
			return cfg.Source, nil
		}
		return &nameSource{key: key, words: words}, nil
	}
	if cfg.Source != nil && cfg.Source.key == key {
		return cfg.Source, nil
	}
	return newGeneratedSource(key)
}

func discordSource(cfg discordConfig) (*nameSource, error) {
	if err := validateDiscordSource(cfg); err != nil {
		return nil, err
	}
	key := sourceKey{kind: "discord", alphabet: cfg.Alphabet, pattern: cfg.Pattern, hash: cfg.SourceHash, path: cfg.SourcePath, length: cfg.Length, seed: cfg.Seed}
	if cfg.SourcePath != "" {
		words, err := readDiscordWordlist(cfg.SourcePath)
		if err != nil {
			return nil, err
		}
		if wordlistHash(words) != cfg.SourceHash {
			return nil, errors.New("Discord wordlist changed; reuse its original contents or choose a new -out")
		}
		if cfg.Source != nil && cfg.Source.key == key {
			return cfg.Source, nil
		}
		return &nameSource{key: key, words: words}, nil
	}
	if cfg.Source != nil && cfg.Source.key == key {
		return cfg.Source, nil
	}
	return newGeneratedSource(key)
}

func sourceCount(cfg config) (int, error) {
	s, err := githubSource(cfg)
	if err != nil {
		return 0, err
	}
	return s.Len(), nil
}
