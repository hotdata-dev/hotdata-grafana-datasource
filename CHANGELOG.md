# Changelog

## 1.0.1

- Build backend binaries with Go 1.27.1, clearing the Go standard-library vulnerabilities reported by govulncheck against the 1.26.5-built release (GO-2026-5026, GO-2026-5942, GO-2026-5972, GO-2026-6088 through 6091, GO-2026-6218).
- Never retry `POST /v1/query` on 502/503/504 — a retried response loss could re-execute SQL; only rate-limited (429) requests and idempotent GETs are retried.
- Cap decoded results at 1,000,000 rows with a panel warning, so a runaway query cannot exhaust plugin memory.
- Bound per-request query parallelism to 10 concurrent upstream queries.
- Multi-value template variables now expand to an escaped, quoted SQL list; hidden queries are no longer executed.
- `$__timeFilter` expands parenthesized, so it composes correctly under `NOT`/`OR`.
- Surface time-series conversion failures as a panel warning instead of silently returning the long frame.
- Honor HTTP-date `Retry-After` headers; fail fast on malformed query-run status responses.
- Mock API: each query now gets an independent async run and result lifecycle.

## 1.0.0

Initial release.

- SQL queries against Hotdata instant databases with sync and async execution (202 + poll), Arrow IPC result decoding, and rate-limit retry
- Table and time-series formats with automatic long-to-wide conversion
- Macros: `$__timeFilter`, `$__timeFrom`, `$__timeTo`, `$__timeGroup` (via `date_bin`), `$__interval`, `$__interval_ms`
- Query editor with database picker and SQL completion for macros, tables, and columns
- Dialect selection: HotSQL, PostgreSQL, DuckDB, Snowflake
- Alerting and template variable support
- Health check that validates credentials and warms idle workspace workers
