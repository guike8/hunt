package main

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func TestDiscordIndexedSourcePreservesLegacyFilteredShuffle(t *testing.T) {
	for _, tc := range []struct{ alphabet, pattern string }{
		{"ab0_.", ""}, {"ab0_.", "a?0?"}, {"ab0_.", "?.??"}, {"ab0_.", "ab0_"},
	} {
		for _, seed := range []int64{1, 42, -17} {
			cfg := discordConfig{Version: 1, Platform: "discord", Length: 4, Alphabet: tc.alphabet, Pattern: tc.pattern, Seed: seed}
			got, err := discordSource(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			total := 1
			for i := 0; i < cfg.Length; i++ {
				total *= len(cfg.Alphabet)
			}
			for value := 0; value < total; value++ {
				name := make([]byte, cfg.Length)
				index := value
				matches := true
				for j := len(name) - 1; j >= 0; j-- {
					name[j] = cfg.Alphabet[index%len(cfg.Alphabet)]
					index /= len(cfg.Alphabet)
					if cfg.Pattern != "" && cfg.Pattern[j] != '?' && cfg.Pattern[j] != name[j] {
						matches = false
					}
				}
				if matches && !strings.Contains(string(name), "..") {
					want = append(want, string(name))
				}
			}
			rand.New(rand.NewSource(seed)).Shuffle(len(want), func(i, j int) { want[i], want[j] = want[j], want[i] })
			if !reflect.DeepEqual(got.materialize(), want) {
				t.Fatalf("alphabet=%q pattern=%q seed=%d changed legacy order", tc.alphabet, tc.pattern, seed)
			}
		}
	}
}

func TestGeneratedSourceCacheReusedAndConfigurationBound(t *testing.T) {
	cfg, err := configureSource(config{Version: 1, Length: 4, Alphabet: "ab01", Seed: 42}, "", "a???")
	if err != nil {
		t.Fatal(err)
	}
	source, err := githubSource(cfg)
	if err != nil || source != cfg.Source || source.Len() != 64 {
		t.Fatalf("cache not reused: source=%p cached=%p error=%v", source, cfg.Source, err)
	}
	cfg.Seed++
	changed, err := githubSource(cfg)
	if err != nil || changed == source {
		t.Fatalf("changed configuration reused stale cache: %v", err)
	}
	if persistentConfig(cfg).Source != nil {
		t.Fatal("runtime source leaked into persistent configuration")
	}
	dcfg, err := configureDiscordSource(discordConfig{Version: 1, Platform: "discord", Length: 4, Alphabet: "ab_.", Seed: 42}, "", "a???")
	if err != nil {
		t.Fatal(err)
	}
	dsource, err := discordSource(dcfg)
	if err != nil || dsource != dcfg.Source {
		t.Fatalf("Discord cache not reused: %v", err)
	}
	if persistentDiscordConfig(dcfg).Source != nil {
		t.Fatal("runtime Discord source leaked into persistent configuration")
	}
}

var benchmarkSource *nameSource
var benchmarkNames []string
var benchmarkName string

func BenchmarkGitHubSourceStartup(b *testing.B) {
	for _, tc := range []struct{ name, pattern string }{{"4Alnum", ""}, {"Pattern", "a?0?"}} {
		cfg := config{Version: 1, Length: 4, Alphabet: "abcdefghijklmnopqrstuvwxyz0123456789", Seed: 1, Pattern: tc.pattern}
		b.Run(tc.name+"/Indexed", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				source, err := githubSource(cfg)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkSource = source
			}
		})
		b.Run(tc.name+"/Materialized", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				names, err := discoveryNames(cfg)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkNames = names
			}
		})
		b.Run(tc.name+"/Cached", func(b *testing.B) {
			var err error
			cfg.Source, err = githubSource(cfg)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchmarkSource, err = githubSource(cfg)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkGitHubSourceNext(b *testing.B) {
	source, err := githubSource(config{Version: 1, Length: 4, Alphabet: "abcdefghijklmnopqrstuvwxyz0123456789", Seed: 1})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkName = source.At(i % source.Len())
	}
}
