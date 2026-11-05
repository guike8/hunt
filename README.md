<p align="center">
  <img src="assets/hero.svg" alt="hunt — Small names. Serious search. GitHub and Discord username discovery." width="100%">
</p>

<p align="center">
  <strong>Batched GitHub discovery · Token-free Discord checks · Durable resume</strong><br>
  A focused Go CLI for finding usernames on GitHub and Discord.
</p>

<p align="center">
  <a href="https://github.com/guike8/hunt/actions/workflows/ci.yml"><img src="https://github.com/guike8/hunt/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/guike8/hunt/releases/latest"><img src="https://img.shields.io/github/v/release/guike8/hunt?color=6debb5" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-6debb5" alt="MIT license"></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#github">GitHub</a> ·
  <a href="#discord">Discord</a> ·
  <a href="#optional-github-rename">Opt-in rename</a> ·
  <a href="#stop-and-continue">Resume</a> ·
  <a href="COMPARISON.md">GitHub benchmarks &amp; comparison</a>
</p>

<p align="center">
  <img src="assets/terminal.svg" alt="Illustrative hunt commands for GitHub and Discord. No live availability results are shown." width="100%">
</p>

## Quick start

[Download a release](https://github.com/guike8/hunt/releases/latest) for macOS, Linux, or Windows, or build with **Go 1.22+**. Release archives have SHA-256 checksums. The Go code uses only the standard library.

```sh
git clone https://github.com/guike8/hunt.git
cd hunt

# GitHub: stop after the first verified available name.
go run . -platform github -n 0 -find 1

# Discord: no token, cookies, or GitHub login needed.
go run . -platform discord -length 3 -n 100 -find 1
```

`github` is the default platform. GitHub discovery needs `GH_TOKEN`, `GITHUB_TOKEN`, or an active [GitHub CLI](https://cli.github.com/) login, in that order. Use `gh auth login --hostname github.com` if needed. **Discord checks are unauthenticated** and never use those credentials.

To build a local binary:

```sh
go build -o hunt .
./hunt -platform discord -n 100 -find 1
```

## Pick a provider

| | GitHub | Discord |
| --- | --- | --- |
| Generated names | 3–4 characters | 2–4 characters; default 3 |
| Wordlists | Match selected length and alphabet | Valid 2–32 character names; mixed lengths allowed |
| Availability method | Batched owner lookup, then optional signup validation | Direct username-availability response |
| Authentication | Token or active `gh` login for discovery; private cookie export for optional renaming | None |
| Default journal | `results.jsonl` | `discord-results.jsonl` |
| Stdout | Candidates in discovery-only mode; verified available names with `-find`, `-live-verify`, or `-verify` | Names explicitly reported available |

Both providers support ordered wordlists, `?` patterns, deterministic generation, `-digits`, `-n`, `-find`, `-fresh`, `-wait`, and separate `-out` files. `-n` limits new checks in each search pass; a `-wait` retry can start another pass. `-n 0` selects all remaining names.

Availability is a timestamped observation, not a reservation. An available name may not exist in your selected space. **Finder mode is the default:** it checks names without creating accounts or changing usernames. [GitHub renaming](#optional-github-rename) is a separate, explicit opt-in; Discord remains finder-only.

## GitHub

Hunt groups **100 owner lookups per request by default**, covering personal and organization names. Opt into larger requests with `-batch 500` (the maximum); request starts remain serial and obey the same provider pacing. Larger batches can increase response latency, so the default remains 100. `-find` streams candidates into verification while discovery continues and cancels after the requested number of explicitly available names.

```sh
# First five verified available names:
go run . -n 0 -find 5 >> github-available.txt

# Discovery only; output contains unverified candidates:
go run . -n 0 >> github-candidates.txt

# Target a pattern (? matches one character):
go run . -pattern 'a??' -n 0 -find 1 -out a-names.jsonl -verify-out a-verified.jsonl

# Wordlist: normalize case, remove duplicates, preserve order:
go run . -input github-names.txt -n 0 -find 1 -out words.jsonl -verify-out words-verified.jsonl

# Four letters, optionally including digits:
go run . -length 4 -digits -n 1000 -out four.jsonl >> four.txt

# Verify all candidates while discovery continues:
go run . -n 0 -live-verify >> github-available.txt
```

A null owner is only a **candidate**: the name may be reserved or restricted. GitHub signup validation accepts only an exact, name-specific success response. Explicit rejection becomes `unavailable`; unknown pages, schema changes, and challenges make no availability claim.

Verify a saved list or discovery journal without API authentication:

```sh
go run . -verify github-candidates.txt -verify-out verified.jsonl >> github-available.txt
go run . -verify results.jsonl -verify-out verified.jsonl >> github-available.txt
```

Discovery starts requests at least one second apart. A complete three-letter scan covers **17,576 combinations in 176 successful requests at the default batch size of 100**. Verification starts at least seven seconds apart and is usually the bottleneck for candidate-heavy searches. With `-live-verify`, the verification backlog grows on demand up to the selected source size, so a slow validator does not impose a small fixed queue on discovery. `-find` keeps a small bounded backlog to stop promptly when its goal is reached.

Generated sources retain shuffled integer positions and create each name when needed. Retries reuse that immutable order; patterns expand only their wildcards. Existing seeds and journal identities retain their order, and wordlist contents are revalidated before reuse.

The signup validator is undocumented and may change. Unknown responses stop verification; `-wait` retries only recognized server cooldowns and exhausted temporary connection/server failures. Authentication, challenges, unexpected schemas, and persistence failures stop the command.

## Optional GitHub rename

`-claim` opts into changing **your existing GitHub account's username** to the first freshly verified available name, then stopping. It requires both the expected current login and your own exported GitHub session-cookie file. Discovery still uses its normal API authentication; an API token does not enable renaming.

```sh
go run . -platform github -n 0 -find 1 \
  -claim \
  -claim-account YOUR_CURRENT_LOGIN \
  -claim-cookies /path/to/private-export.json
```

Add `-input github-names.txt` to search your own ordered list. Claim mode handles one three- or four-character name and rejects `-find` values above one, `-verify`, and `-live-verify`. It also rejects `-wait` and `-status-file`. All claim flags are GitHub-only and rejected with Discord.

Supply your own **JSON or Netscape-format cookie export**. Keep it private and outside the repository; never commit or upload it. On macOS/Linux, the file must have `0600` permissions, for example `chmod 600 /path/to/private-export.json`. Hunt reads only that explicitly supplied file; it does not extract cookies from browser storage or use a GitHub CLI credential for renaming.

GitHub provides no official username-rename API. This optional path reads the account settings form and CSRF token, checks that the authenticated login matches `-claim-account`, and submits one rename request after a fresh availability check. Discovery is cancelled before submission. Hunt then reads the account settings again and prints the name only after confirming the new login. An ambiguous submission is never retried automatically; check your account before another attempt.

Renaming releases the previous handle and changes the profile URL. Read [GitHub's username-change effects](https://docs.github.com/en/account-and-profile/concepts/username-changes) before enabling it.

**Claim-path tests are offline and use synthetic sessions only.** No live account rename was tested. The website form can change, and the command may stop for reauthentication, an unsupported form, or an unconfirmed outcome. Finder commands remain available independently.

## Discord

Discord checks each name directly, without a token or cookies. Only an explicit boolean `taken` response is accepted: `false` becomes `available`, and `true` becomes `unavailable`. Missing, null, malformed, or error responses remain unknown.

```sh
# Two-character search; stop after the first available observation:
go run . -platform discord -length 2 -n 0 -find 1 >> discord-available.txt

# Include digits, underscores, and periods in generated names:
go run . -platform discord -length 3 -digits -symbols -n 100

# A targeted pattern; symbols in patterns need -symbols:
go run . -platform discord -pattern 'a??' -n 100 -find 1 -out discord-a.jsonl

# Mixed-length wordlist of valid 2–32 character names:
go run . -platform discord -input discord-names.txt -n 0 -find 1 -out discord-words.jsonl

# A slower request cadence:
go run . -platform discord -delay 3s -n 100
```

Generated names use lowercase letters by default. `-digits` adds `0–9`, and Discord-only `-symbols` adds `_` and `.`. Wordlists accept the full valid character set regardless of generation flags, normalize case, and reject consecutive periods. See [Discord's username rules](https://support.discord.com/hc/en-us/articles/12620128861463-New-Usernames-Display-Names).

Discord starts requests at least one second apart by default; `-delay` can make that interval longer. Server rate headers and retry information can extend the wait, and cooldowns persist across restarts. See [Discord's rate-limit documentation](https://docs.discord.com/developers/topics/rate-limits).

The availability endpoint is undocumented: `POST /api/v9/unique-username/username-attempt-unauthed`. Its accepted response is an observation, not a guarantee that a name can be claimed. Hunt stops on unknown responses and does not attempt authentication or account changes.

`-batch`, `-verify`, `-verify-out`, `-live-verify`, and all `-claim*` options belong to GitHub and are rejected with Discord. `-symbols` and `-delay` belong to Discord.

## Stop and continue

Press **Ctrl-C**, then repeat the same command to resume. Default journals are separate for each provider. Use `>>` to retain prior text output; JSONL journals hold the authoritative history.

To keep searching through known server cooldowns and temporary connection/server failures, enable search-only `-wait`:

```sh
go run . -length 3 -digits -n 0 -live-verify -wait >> github-available.txt
go run . -platform discord -length 4 -digits -n 0 -wait >> discord-available.txt
```

`-wait` resumes completed checks after each pause, including observations whose short positive-cache lifetime has elapsed. GitHub discovery can wait through an API cooldown while verification drains saved candidates. Authentication failures, challenges, unknown responses, and journal failures still stop the search. `-wait` cannot be combined with `-fresh` or `-claim`.

Verified available output has a durable publication acknowledgment. An available observation saved before an interrupted output write is replayed on resume. Regular-file stdout is synced before its acknowledgment is saved. A crash after output succeeds but before acknowledgment can still duplicate a line; consumers should deduplicate by platform and username. Pipes and terminals are acknowledged after their writer accepts the line, without promising delivery by downstream software.

Sources are bound to their journal: changing the wordlist contents, pattern, alphabet, length, or seed requires a new `-out` file. GitHub replays pending verification first. Durable provider state coordinates request timing and server cooldowns across hunt processes, even when they use different result journals. GraphQL discovery, GitHub signup validation, and Discord eligibility have separate pacing state.

By default, provider state lives under the operating system's user cache directory in `hunt/providers`. Set `HUNT_STATE_DIR` to choose a persistent directory; keep all hunt processes pointed at the same directory so they share pacing. Result journals also preserve timing checkpoints for migration and resume.

Journals use exclusive operating-system locks. Process exit releases ownership automatically; leftover `.lock` files are harmless. Torn final writes after a valid record recover, while committed corruption and unrecognized files are rejected.

Negative observations are cached for 24 hours and positive ones for 30 seconds. To recheck while preserving history:

```sh
go run . -fresh -n 0 -find 1
go run . -platform discord -fresh -n 100 -find 1
go run . -fresh -verify results.jsonl >> github-available.txt
```

`-fresh` rechecks selected names on each invocation; omit it for normal resume. It preserves request spacing and cooldowns. Finder checks do not rotate proxies or read browser session storage.

Progress goes to stderr. Use `-quiet` to hide progress, `-no-color` or `NO_COLOR` for plain text, and `-version` to print the build version. `go run . -help` lists every option.

## Machine-readable progress

`-status-file` optionally writes an atomic JSON snapshot once per second and at command completion:

```sh
go run . -n 0 -live-verify -wait -status-file github-status.json >> github-available.txt
```

The snapshot separates `discovery_checked`, `discovery_requests`, `verification_checked`, `verification_requests`, and `pending_verification`, with `source_total`, platform, PID, timestamps, and `state` (`running`, `complete`, `stopped`, or `failed`). Discord's direct availability requests use the verification stage. `last_progress_at` advances when a completion counter advances, while `updated_at` advances during idle waits too. A `running` snapshot may therefore represent a normal server cooldown.

`discovery_checked` counts completed source positions, including loaded journal baselines. Its name rate counts newly completed positions during this invocation, so it can stay unchanged while `-fresh` rechecks previously completed names. Verification counters include loaded conclusive baselines and new observations; their name rate counts new conclusive observations during this invocation. `available` counts available observations, including saved baselines and new confirmations; repeated rechecks can add confirmations of the same name. All rates are invocation averages, and request counts include attempted network requests, including retries. These are display metrics, not a list of currently free names or an authoritative history; use the result journals for exact usernames and check times.

Snapshots omit credentials, account session data, raw provider responses, and error messages. The file must differ from every input, result, verification, and cookie path. The containing directory must already exist. `-status-file` cannot be used with `-claim`. A later snapshot-write failure does not stop the search; an old `updated_at` can indicate stale display data.

## Measured GitHub performance

A fixed live **GitHub-only** test checked the same 20 names with matching classifications:

| Method | Requests | Elapsed |
| --- | ---: | ---: |
| Serial REST baseline, one-second start cadence | 20 | 19.23 s |
| One GraphQL batch | 1 | 0.77 s |

These numbers do not describe Discord. They cover that GitHub workload and its deliberately paced baseline; no competing executable was benchmarked. See the [11-repository GitHub comparison](COMPARISON.md), [raw microbenchmarks](benchmark-results.txt), and [live test data](live-benchmark.json).

For a generated four-character alphanumeric source of 1,679,616 names, local startup benchmarks on an Apple M5 measured median allocated bytes of **47,046,248 → 13,449,800** and startup time of **52.276 ms → 13.999 ms** for materialized versus indexed generation. This measures source construction with no network requests, not end-to-end search speed or peak resident memory. See [method and allocation details](COMPARISON.md#source-startup-and-journal-replay-v030).

## Development

```sh
make check
make build
# Or directly:
go test -race ./...
go vet ./...
go test -run '^$' -bench . -benchmem
```

Builds target macOS, Linux, and Windows. CI tests those platforms and the minimum Go version. Tests cover provider isolation, explicit availability responses, cancellation, retries, seeded source-order compatibility, refresh, journal recovery, cross-process locking and pacing, output replay, status snapshots, and resume. Optional rename tests use offline synthetic cookie and form fixtures only.

`make build VERSION=<tag>` embeds a version in a compact binary. `make release VERSION=<tag>` creates six deterministic platform archives and a checksum manifest using Python's standard library. All implementation code was written independently.

Released under the [MIT license](LICENSE).
