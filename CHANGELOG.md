# Changelog

## 1.0.1

- Build backend binaries with Go 1.27.1, clearing the Go standard-library vulnerabilities reported by govulncheck against the 1.26.5-built release (GO-2026-5026, GO-2026-5942, GO-2026-5972, GO-2026-6088 through 6091, GO-2026-6218). No functional changes.

## 1.0.0

Initial release.

- SQL queries against Hotdata instant databases with sync and async execution (202 + poll), Arrow IPC result decoding, and rate-limit retry
- Table and time-series formats with automatic long-to-wide conversion
- Macros: `$__timeFilter`, `$__timeFrom`, `$__timeTo`, `$__timeGroup` (via `date_bin`), `$__interval`, `$__interval_ms`
- Query editor with database picker and SQL completion for macros, tables, and columns
- Dialect selection: HotSQL, PostgreSQL, DuckDB, Snowflake
- Alerting and template variable support
- Health check that validates credentials and warms idle workspace workers
