# Changelog

## v0.3.0 (unreleased)

- Added search-only `-wait` recovery for known server cooldowns and exhausted temporary connection/server failures. Completed checks survive retry passes; authentication, challenges, unknown schemas, and persistence errors still stop the search.
- Generate names from cached integer positions instead of materializing the full string namespace. Preserve seeded GitHub and Discord order, direct wildcard expansion, ordered wordlists, and source-bound journal compatibility.
- Allow opt-in GraphQL batches up to 500 names with `-batch 500`; keep the default at 100 and preserve serial provider pacing.
- Grow the full-verification backlog as needed up to the selected source size; retain a small bounded queue for stop-after-N searches. Keep API cooldown recovery inside discovery so verification can continue independently.
- Coordinate durable request cadence and server cooldowns across processes with shared provider state, optionally located with `HUNT_STATE_DIR`.
- Acknowledge verified available output only after delivery to the writer, syncing regular-file output first. Replay unacknowledged positive observations after interruption; a crash between output and acknowledgment may still duplicate a line.
- Added optional atomic `-status-file` JSON snapshots with separate discovery and verification counters, rates, backlog, timestamps, and final state. Status files exclude credentials and raw error/provider text; claim mode and protected-path collisions are rejected.
- Reduce discovery-journal replay bookkeeping by retaining ordered detailed state only for names that have ever been candidates. Added startup, allocation, replay, progress, and concurrency regression coverage.

## v0.2.0

- Added a separate Discord provider with strict eligibility response parsing, source-bound journals, refresh controls, and persisted fractional rate cooldowns.
- Added explicit opt-in GitHub claiming. It requires a user-supplied session-cookie file and expected account, and submits at most one rename after availability verification. The rename path is tested offline with synthetic sessions; no real account rename was used for testing.
- Fixed Windows recovery of incomplete verification-journal writes.
- Updated the CLI, README, and artwork for both providers.
- Added CI concurrency handling to cancel superseded branch runs.

## v0.1.0

- Initial GitHub finder: grouped GraphQL lookups, streaming verification, stop-after-N results, patterns, ordered wordlists, refresh, and recoverable journals.
- Added branded terminal output, quiet/color controls, tests, benchmarks, and six platform release archives.
