# Hotdata datasource for Grafana

Grafana backend datasource plugin for [Hotdata](https://hotdata.dev). Runs SQL against Hotdata instant databases and returns results Arrow-native — the plugin decodes the query result's Arrow IPC stream straight into Grafana data frames, so types survive end to end.

Design and wire contract: [SPEC.md](./SPEC.md).

## Status

M1–M3 complete (see [SPEC.md](./SPEC.md) milestones); M4 remaining item is catalog submission.

- Config editor: API URL, API key (encrypted), workspace ID, default database, default dialect.
- Query editor: SQL editor with completion (macros, tables, columns via the schema resources), database picker, format (table / time series), per-query dialect override.
- Sync and async query flows (`POST /v1/query` → 202 → poll `GET /v1/query-runs/{id}`), 429 retry with `Retry-After`.
- Arrow IPC → data frame conversion for ints, floats, decimals, strings, bools, timestamps, dates, binary; nested types render as strings.
- Macros: `$__timeFilter(col)`, `$__timeFrom()`, `$__timeTo()`, `$__timeGroup(col, interval)` (via `date_bin`), `$__interval`, `$__interval_ms`.
- `format: timeseries` long→wide conversion for multi-series panels.
- Health check that validates key + workspace and warms the workspace worker.
- Alerting verified end-to-end (rule on plugin query → firing instances with per-series labels).
- Resource endpoints: `databases`, `workspaces`, `schemas`, `tables`, `columns`.
- Provisioned demo dashboard (`provisioning/dashboards/hotdata-demo.json`).

## Development

```bash
npm install
npm run dev                 # frontend, watch mode
mage -v build:linuxARM64    # backend (for the docker Grafana; use build:darwinARM64 for local go tests)
```

### Run (no credentials needed)

`docker compose up` starts Grafana **and** the bundled mock Hotdata API, with the provisioned datasource pointing at the mock — the demo dashboard works out of the box.

```bash
docker compose up -d
open http://localhost:3000
```

The mock implements the captured v1 wire contract (sync + async flows, Arrow results, discovery) and serves a demo time series. SQL containing the word `slow` exercises the async path.

### Run against a live workspace

Set all four variables. `HOTDATA_API_URL` must be exported — if left unset it defaults to the mock; an explicitly empty value falls back to `https://api.hotdata.dev`:

```bash
export HOTDATA_API_URL=                # empty = https://api.hotdata.dev
export HOTDATA_API_KEY=hd_...          # workspace API key
export HOTDATA_WORKSPACE_ID=work...
export HOTDATA_DATABASE_ID=dbid...     # default database
docker compose up -d
```

### Tests

```bash
go test ./...            # backend: contract fixtures, arrow decode, macros, health
npm run e2e              # Playwright against the running docker Grafana + mock API
```
