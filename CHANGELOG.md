# Changelog

## v0.2.0

- Added a separate Discord provider with strict eligibility response parsing, source-bound journals, refresh controls, and persisted fractional rate cooldowns.
- Added explicit opt-in GitHub claiming. It requires a user-supplied session-cookie file and expected account, and submits at most one rename after availability verification. The rename path is tested offline with synthetic sessions; no real account rename was used for testing.
- Fixed Windows recovery of incomplete verification-journal writes.
- Updated the CLI, README, and artwork for both providers.
- Added CI concurrency handling to cancel superseded branch runs.

## v0.1.0

- Initial GitHub finder: grouped GraphQL lookups, streaming verification, stop-after-N results, patterns, ordered wordlists, refresh, and recoverable journals.
- Added branded terminal output, quiet/color controls, tests, benchmarks, and six platform release archives.
