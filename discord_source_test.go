package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testDiscordConfig() discordConfig {
	return discordConfig{Version: 1, Platform: "discord", Length: 3, Alphabet: "abcdefghijklmnopqrstuvwxyz", Seed: 1}
}

func TestDiscordGeneratedSourcesAreUniqueValidAndDeterministic(t *testing.T) {
	cfg := testDiscordConfig()
	cfg.Alphabet = "a."
	names, err := discordNames(cfg)
	if err != nil || len(names) != 5 {
		t.Fatalf("names=%v error=%v", names, err)
	}
	seen := make(map[string]bool)
	for _, name := range names {
		if seen[name] || !validDiscordName(name) || strings.Contains(name, "..") {
			t.Fatalf("invalid or repeated generated name %q", name)
		}
		seen[name] = true
	}
	again, err := discordNames(cfg)
	if err != nil || !reflect.DeepEqual(names, again) {
		t.Fatal("Discord seeded order changed")
	}
	cfg.Pattern = "a.?"
	names, err = discordNames(cfg)
	if err != nil || !reflect.DeepEqual(names, []string{"a.a"}) {
		t.Fatalf("pattern did not filter invalid periods: names=%v error=%v", names, err)
	}
	for _, length := range []int{1, 5, 32} {
		cfg.Pattern, cfg.Length = "", length
		if _, err := discordNames(cfg); err == nil {
			t.Fatalf("unbounded generated length %d accepted", length)
		}
	}
}

func TestDiscordWordlistSupportsMixedLengthsAndBindsOrderedContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.txt")
	longName := strings.Repeat("a", 32)
	if err := os.WriteFile(path, []byte(" AB\nname_1.test\nAB\n"+longName+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := configureDiscordSource(testDiscordConfig(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	names, err := discordNames(cfg)
	if err != nil || !reflect.DeepEqual(names, []string{"ab", "name_1.test", longName}) || cfg.Length != 0 || cfg.SourceHash == "" {
		t.Fatalf("mixed wordlist: names=%v cfg=%+v error=%v", names, cfg, err)
	}
	if err := os.WriteFile(path, []byte(longName+"\nab\nname_1.test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := discordNames(cfg); err == nil {
		t.Fatal("changed source order accepted")
	}
	for _, invalid := range []string{"a", strings.Repeat("a", 33), "bad..name", "bad-name", "日本"} {
		if err := os.WriteFile(path, []byte(invalid+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := configureDiscordSource(testDiscordConfig(), path, ""); err == nil {
			t.Fatalf("invalid wordlist name accepted: %q", invalid)
		}
	}
}

func TestPlatformFlagsDoNotMisrouteServiceSpecificOptions(t *testing.T) {
	for _, name := range []string{"verify", "verify-out", "batch", "live-verify"} {
		if err := validatePlatformFlags("discord", map[string]bool{name: true}); err == nil {
			t.Fatalf("Discord accepted GitHub-only -%s", name)
		}
	}
	for _, name := range []string{"symbols", "delay"} {
		if err := validatePlatformFlags("github", map[string]bool{name: true}); err == nil {
			t.Fatalf("GitHub silently accepted Discord-only -%s", name)
		}
	}
	if err := validatePlatformFlags("github", nil); err != nil {
		t.Fatal(err)
	}
	if err := validatePlatformFlags("discord", map[string]bool{"find": true, "input": true, "fresh": true}); err != nil {
		t.Fatal(err)
	}
	if err := validatePlatformFlags("unknown", nil); err == nil {
		t.Fatal("unknown platform accepted")
	}
}
