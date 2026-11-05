package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// This benchmark isolates client CPU/allocation and request count. It simulates
// neither GitHub latency nor competitors' executable performance.
func BenchmarkOwnerLookupBatches(b *testing.B) {
	names := make([]string, 100)
	for i := range names {
		names[i] = fmt.Sprintf("n%02d", i)
	}
	for _, size := range []int{1, 100} {
		b.Run(fmt.Sprintf("batch_%d", size), func(b *testing.B) {
			requests := 0
			client := &githubClient{http: &http.Client{Transport: finderTestTransport(func(req *http.Request) (*http.Response, error) {
				requests++
				var body struct{ Query string }
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return nil, err
				}
				data := map[string]any{"rateLimit": map[string]any{"remaining": 4000}}
				for i := 0; i < strings.Count(body.Query, "repositoryOwner("); i++ {
					data[fmt.Sprintf("u%d", i)] = nil
				}
				encoded, _ := json.Marshal(map[string]any{"data": data})
				return finderTestResponse(200, string(encoded), nil), nil
			})}}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				for start := 0; start < len(names); start += size {
					if _, err := client.lookup(context.Background(), names[start:start+size]); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(requests)/float64(b.N), "requests/100names")
		})
	}
}

// Both cases durably save the same 100 observations. The batch case has one
// write/fsync; the single case has one per observation.
func BenchmarkDurableJournalBatches(b *testing.B) {
	for _, batch := range []bool{false, true} {
		b.Run(fmt.Sprintf("batch_%t", batch), func(b *testing.B) {
			cfg := config{Version: 1, Length: 3, Alphabet: "abcdefghijklmnopqrstuvwxyz", Seed: 1}
			store, err := openStore(filepath.Join(b.TempDir(), "results.jsonl"), cfg)
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			rows := make([]lookupResult, 100)
			for i := range rows {
				rows[i] = lookupResult{Name: nameAt(i, cfg), Status: "taken"}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				if batch {
					err = store.appendBatch(rows)
				} else {
					for _, row := range rows {
						if err = store.append(row); err != nil {
							break
						}
					}
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
