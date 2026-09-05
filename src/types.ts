import { DataSourceJsonData } from '@grafana/data';
import { DataQuery } from '@grafana/schema';

export type QueryFormat = 'table' | 'timeseries';
export type Dialect = 'hotsql' | 'postgres' | 'duckdb' | 'snowflake';

export interface HotdataQuery extends DataQuery {
  rawSql: string;
  /** Instant database to run in; falls back to the datasource default. */
  databaseId?: string;
  format: QueryFormat;
  dialect?: Dialect;
}

export const DEFAULT_QUERY: Partial<HotdataQuery> = {
  rawSql: '',
  format: 'table',
};

export interface HotdataDataSourceOptions extends DataSourceJsonData {
  /** Hotdata API base URL. Defaults to https://api.hotdata.dev */
  apiUrl?: string;
  workspaceId?: string;
  defaultDatabaseId?: string;
  defaultDialect?: Dialect;
}

export interface HotdataSecureJsonData {
  /** Workspace API key (hd_…). Stored encrypted; only the backend sees it. */
  apiKey?: string;
}

/** Shape served by the backend `databases` resource endpoint. */
export interface DatabaseResource {
  id: string;
  name: string;
  default_catalog: string;
  default_schema: string;
}
