# Public comparison and measured improvements

Research date: November 5, 2026. Scope: ten GitHub-specific public repositories and one multi-platform control. Original READMEs and repository metadata were reviewed; their source code was not copied and their executables were not run. OpenCLI was attempted first, then fallback browsing was used because its browser bridge failed.

This establishes concrete feature and request-efficiency improvements over a bounded sample. It does not establish superiority over every public repository. **Not documented** below means exactly that; it is not proof a feature is absent from the code.

## Measurements

### Live, fixed-name comparison

Twenty identical three-letter names (`aaa` through `aat`) were checked using the same Go HTTP client, credentials, and host:

| Method | Requests | Elapsed | Classification agreement |
| --- | ---: | ---: | --- |
| Serial REST baseline, one-second request-start cadence | 20 | 19.232 s | 20/20 |
| One GraphQL owner batch | 1 | 0.773 s | 20/20 |

That run reduced requests by 95% and elapsed time by about 24.9 times. The baseline deliberately uses the earlier script's one-second cadence; this is **not** a claim that GraphQL is always 24.9 times faster than unpaced REST, or that another repository's executable was beaten by this factor. Timing is one network observation. See [the frozen names and raw data](live-benchmark.json).

### Repeatable local benchmarks

macOS arm64, Apple M5, three repetitions, 500 ms benchmark target. The cases process the same 100-name workload:

| Metric | Single-name operations | 100-name batch | Meaning |
| --- | ---: | ---: | --- |
| Transport requests | 100 | 1 | 100 times fewer requests per 100 names |
| Median client CPU time, fake transport | 1.017 ms | 0.225 ms | No real network latency simulated |
| Median allocation bytes | 689,577 | 87,320 | About 7.9 times less allocated memory |
| Median durable journal time | 382.010 ms | 4.040 ms | About 94.6 times faster on this filesystem |

The journal cases each save 100 observations; the baseline syncs every observation, while the batch case performs one write and sync before publishing any candidate. The filesystem numbers are local measurements, not portable guarantees. [Raw benchmark output](benchmark-results.txt) and [benchmark code](benchmark_test.go) are included.

```sh
go test -run '^$' -bench . -benchmem -benchtime=500ms -count=3
```

### Source startup and journal replay (v0.3.0)

Local macOS arm64 measurements on an Apple M5, three repetitions of each case with `-benchtime=1x`. No HTTP requests were made. Values below are medians; allocated bytes are Go benchmark allocations per operation, not peak live memory or resident process size.

| Source startup | Materialized names | Indexed names | Reused indexed source |
| --- | ---: | ---: | ---: |
| Four alphanumeric characters: elapsed | 52.276 ms | 13.999 ms | 3.875 µs |
| Four alphanumeric characters: allocated bytes | 47,046,248 | 13,449,800 | 1,112 |
| Four alphanumeric characters: allocations | 1,679,631 | 14 | 7 |
| Pattern `a?0?`: elapsed | 66.959 µs | 35.375 µs | 3.833 µs |
| Pattern `a?0?`: allocated bytes | 44,520 | 17,576 | 1,112 |
| Pattern `a?0?`: allocations | 1,309 | 12 | 7 |

The full source contains 1,679,616 names from `a-z0-9`; the pattern contains 1,296. Both paths use the same seeded permutation and yield identical names in identical order. The materialized comparison constructs the strings upfront; the indexed source retains integer positions and creates names as batches consume them. This reduced startup allocation bytes by 71.4% for that full source, with later per-name string allocation deferred to consumption. Retry passes reuse the generated source, while wordlists are still read and checked against their bound hash.

A synthetic 100,000-observation discovery journal, containing 90,000 taken names and 10,000 candidates, replayed in **170.029 ms** median with **66,964,256 allocated bytes** and **1,100,713 allocations**. The completed-name index still covers every conclusive observation; detailed FIFO history is retained only for names that have ever been candidates. JSON parsing and journal validation remain part of this measurement.

These startup and replay cases do not measure provider throughput, available-name yield, or another repository's executable. See [raw output](optimization-benchmarks.txt), [source benchmark code](source_iter_test.go), and [replay benchmark code](store_test.go).

```sh
go test -run '^$' -bench 'Benchmark(GitHubSourceStartup|StoreReplay100k)$' -benchmem -benchtime=1x -count=3
```

### Opt-in larger GitHub batches

The default remains 100 aliases per request; `-batch` now accepts up to 500. A bounded actual-CLI sample on November 5, 2026 used three-character alphanumeric generation with seed 771103:

| Batch size | Completed lookups | Requests | CLI elapsed | Lookups / elapsed second |
| --- | ---: | ---: | ---: | ---: |
| 100 | 100 | 1 | 2.372 s | 42.2 |
| 500 | 500 | 1 | 8.140 s | 61.4 |

These were single runs with different workload sizes; elapsed time includes source startup and network latency. They support exposing the larger batch as an option, not a universal speedup claim. The 500-name response took longer, and both settings retain serial request starts and provider cooldowns. This does not measure signup verification or Discord. See [saved CLI measurements and limitations](optimization-live-benchmark.json).

## Public implementations inspected

| Repository | Documented useful approach | Concrete response in this finder |
| --- | --- | --- |
| [y0f/github-user-checker](https://github.com/y0f/github-user-checker) | Go, current signup validator, exact response checks, persistent decided-name ledger | Keep strict validation; add supported GraphQL discovery batches, pending-candidate resume, first/N verified finds, freshness controls |
| [byigitt/github-username-checker](https://github.com/byigitt/github-username-checker) | Authenticated REST lookup, word ranking, persistent cache, force refresh | Batch owner lookups; add ordered wordlists, patterns, explicit refresh and distinct verified results |
| [amirhosse1n/namescope](https://github.com/amirhosse1n/namescope) | API then signup validation, graded result labels, persistent history | Preserve distinct candidate/available/unknown semantics in a dependency-free Go CLI with streaming verification |
| [1870s/Github-Username-Checker](https://github.com/1870s/Github-Username-Checker) | Text input, autosave, resume, fixed retry waits | Bind wordlist hashes to journals; respect actual rate headers; never classify an API-null owner as confirmed available |
| [metecan/AvailableUsernameFinder-WebScraping](https://github.com/metecan/AvailableUsernameFinder-WebScraping) | Simple random four-letter Go scraper using Colly | No third-party Go modules; complete duplicate-free generation, batching, verification, recovery |
| [xbubbo/Github-Username-Checker](https://github.com/xbubbo/Github-Username-Checker) | Lookup then browser-cookie rename validation | Finder uses read-only signup validation; the separately enabled GitHub claim flow is not part of these benchmarks |
| [bffr24/GitHub-name-checker](https://github.com/bffr24/GitHub-name-checker) | Exhaustive random three/four-letter generation, multiple availability signals, header-based pauses | Complete generation plus durable pending validation; ambiguous outcomes remain unknown |
| [yTax/Github-Username-Checker](https://github.com/yTax/Github-Username-Checker) | Wordlists, named sessions, web scraping, fixed retry pause | Source-bound resumable journals, GraphQL batches, persisted request cadence and cooldown |
| [DefaultPerson/github-usernames-checker](https://github.com/DefaultPerson/github-usernames-checker) | Go worker pool, retries, wordlist, cookie auto-claim | Batch to reduce request count; bounded verification queue and cancellation. Account-changing features are outside this task |
| [c7md/GUC](https://github.com/c7md/GUC) | Go API checks from a username file | Ordered deduplicated input, grouped lookups, durable resume, actual validator checks |
| [zaan-app/scato](https://github.com/zaan-app/scato) | Multi-platform checks, explicit unknown/rate outcomes, caveats for GitHub page heuristics | GitHub-specific two-stage checking; do not infer availability from an error page |

None of these inspected READMEs documents GraphQL alias batching or an explicit stop-after-N verified-results option. Their existence elsewhere remains possible. This comparison covers finder behavior, not chat notifications, dashboards, or the later opt-in GitHub claim flow. Discord was added after this GitHub-focused comparison and is not covered by these throughput measurements.

## What is now tested

- Both users and organizations are recognized as owners; null owners are candidates only.
- Global schema/auth/rate failures stop requests. Partial alias failures are retried in smaller groups; unresolved names remain unchecked while unrelated names continue.
- A complete discovery batch is durable before candidates reach stdout or the verifier queue.
- Streaming verification uses a growable queue bounded by the selected source for full verification, with a small bound for stop-after-N searches. It cancels discovery after the requested number of explicitly available names.
- Every pending candidate can be replayed after interruption. Wordlist hashes, pattern, alphabet, length and seed prevent incompatible resume.
- Both journals reject concurrent writers. Tests cover a second process and lock release when a process exits without an unlock callback.
- Torn final records after a valid record recover; corrupt committed records are rejected. Complete final JSON without a newline is preserved and remains appendable.
- Provider request timing and rate cooldown survive restart and coordinate across independent processes using the same provider-state directory. Forced refresh bypasses cached observations, not pacing or cooldowns.
- Verified positive observations have durable output acknowledgments and replay after an interrupted output write. Regular files are synced before acknowledgment; crash duplicates remain possible.
- Individual transient retry sequences are bounded. Search-only `-wait` can retry those sequences or wait through known cooldowns; unexpected responses, challenges, and authentication failures still stop. Unknown responses and suggestions for a different name never become available results.
- Indexed generation preserves the exact seeded order for GitHub and filtered Discord names. Status snapshots are atomic and safe under concurrent progress updates; counters separate discovery from verification.
- Race tests and vet pass on macOS. Builds compile for macOS, Linux amd64 and Windows amd64; Linux/Windows runtime behavior was not executed here.

## Limits that code cannot remove

GitHub's registration validator is the throughput bottleneck for confirming candidates. This finder starts those requests at least seven seconds apart and respects longer server cooldowns; increasing the discovery batch size cannot make GitHub confirm more names. `-wait` can continue after a known cooldown without bypassing it. No available three-letter name is guaranteed to exist. A successful response is only a timestamped observation and does not reserve the name.

The validator is undocumented. Its current route and required `Accept: text/fragment+html` header were discovered in [the public checker's notes](https://github.com/y0f/github-user-checker#notes) and independently reproduced. For example, `njv` had no GraphQL owner but returned an explicit unavailable result. The old validator route returned generic 404 pages, which this implementation rejects.

Supported API references: [repositoryOwner query](https://docs.github.com/en/graphql/reference/repos#repositoryowner), [GraphQL rate and resource limits](https://docs.github.com/en/graphql/overview/rate-limits-and-query-limits-for-the-graphql-api).
