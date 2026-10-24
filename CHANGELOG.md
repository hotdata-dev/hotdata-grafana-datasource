# Changelog

## Unreleased

- Read query results as Arrow directly from `POST /v1/query`, so a fast query takes one request instead of two. Async and too-large results still come from the Results API.
- Show the real error for failed queries: read `error_message` from query runs and failed results, and show the `message` and `code` from Hotdata error bodies instead of raw JSON.
- The query editor's database picker searches by name on the server as you type (`GET /v1/databases?search=`), so any database in a large workspace can be found without loading them all. A saved database shows its name, not its id.
- Follow pagination on `/v1/information_schema`, so SQL autocomplete lists every table, not just the first page.
- Handle the Results API's `202` (still persisting) and `409` (failed) responses. Polling keeps its fast 50ms backoff rather than the server's `Retry-After: 1`, which would add a second to large results.
- The health check validates credentials with one `GET /v1/databases?limit=1` instead of listing every database.

## 1.0.4

- Clear the high-severity osv-scanner findings that failed Grafana plugin validation for 1.0.3 (all in dev/build tooling): force basic-ftp to 6.2.3 and source-map-js to 1.2.2, and replace micromatch with picomatch under eslint-webpack-plugin, since braces has no fixed release.

## 1.0.3

- Build backend binaries with Go 1.27.2 and bump golang.org/x/net to v0.60.0, clearing the Go vulnerabilities flagged by Grafana plugin validation (GO-2026-6599, 6600, 6603, 6605, 6607, 6608, 6610 through 6613, 6617).
- Update @grafana packages to 13.1.8 and force moment to 2.31.0 (GHSA-4p3w-j4w9-5jqw).

## 1.0.2

- Bump google.golang.org/grpc to v1.83.2, fixing the high-severity xDS DoS vulnerability CVE-2026-84445 (GHSA-2v4p-qf9q-27wj) flagged by Grafana plugin validation.

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
