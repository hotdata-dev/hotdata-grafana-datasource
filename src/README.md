# Hotdata datasource

Query [Hotdata](https://hotdata.dev) instant databases with SQL from Grafana dashboards, alerts, and template variables. Results transfer Arrow-native from Hotdata's engine into Grafana data frames, so column types survive end to end.

## Features

- Raw SQL editor with completion for macros, tables, and columns from your workspace schema
- Table and time-series formats (long results pivot to one series per label automatically)
- Alerting support (queries run in the plugin backend)
- Dialect choice: HotSQL (native), PostgreSQL, DuckDB, or Snowflake — transpiled server-side
- Handles Hotdata's async query lifecycle, rate-limit backpressure, and idle-workspace cold starts

## Configuration

| Field | Value |
|---|---|
| API URL | `https://api.hotdata.dev` (default) |
| API key | Workspace API key (`hd_…`), stored encrypted |
| Workspace ID | `work…`, from `hotdata workspaces list` |
| Default database ID | `dbid…`, used when a query does not pick one |
| Default SQL dialect | Applied when a query does not override it |

The health check validates the key and workspace, then runs `SELECT 1` against the default database — which also wakes the workspace worker, so the first dashboard load is fast.

## Macros

| Macro | Expands to |
|---|---|
| `$__timeFilter(col)` | `col >= '<from>' AND col <= '<to>'` (RFC 3339 UTC) |
| `$__timeFrom()` / `$__timeTo()` | dashboard range bound as a quoted literal |
| `$__timeGroup(col, interval)` | `date_bin(interval '…', col, timestamp '1970-01-01T00:00:00Z')` |
| `$__interval` / `$__interval_ms` | panel interval (e.g. `30s` / `30000`) |

Example:

```sql
SELECT $__timeGroup(created_at, $__interval) AS time,
       status,
       count(*) AS orders
FROM shop.public.orders
WHERE $__timeFilter(created_at)
GROUP BY 1, 2
ORDER BY 1
```

Set the query format to **Time series** to get one series per `status`.

## Template variables

Query variables run SQL and use the first string column as values (optional second column as labels). Use standard Grafana interpolation in queries: `$var`, `${var:singlequote}`, `${var:csv}`.
