import { CoreApp, DataSourceInstanceSettings, DataSourceVariableSupport, ScopedVars } from '@grafana/data';
import { DataSourceWithBackend, getTemplateSrv } from '@grafana/runtime';

import { DEFAULT_QUERY, HotdataDataSourceOptions, HotdataQuery } from './types';

// Query variables use the plugin's own query editor; values come from the
// first string field of the returned frame.
class HotdataVariableSupport extends DataSourceVariableSupport<DataSource, HotdataQuery, HotdataDataSourceOptions> {}

export class DataSource extends DataSourceWithBackend<HotdataQuery, HotdataDataSourceOptions> {
  constructor(instanceSettings: DataSourceInstanceSettings<HotdataDataSourceOptions>) {
    super(instanceSettings);
    this.variables = new HotdataVariableSupport();
  }

  getDefaultQuery(_: CoreApp): Partial<HotdataQuery> {
    return DEFAULT_QUERY;
  }

  applyTemplateVariables(query: HotdataQuery, scopedVars: ScopedVars): HotdataQuery {
    return {
      ...query,
      rawSql: getTemplateSrv().replace(query.rawSql, scopedVars, interpolateQueryExpr),
    };
  }

  filterQuery(query: HotdataQuery): boolean {
    // Skip hidden queries and empty editors entirely — no request, no polling.
    return !query.hide && !!query.rawSql?.trim();
  }
}

const quoteLiteral = (value: unknown) => "'" + String(value).replace(/'/g, "''") + "'";

// Default variable formatting, mirroring Grafana's SQL datasources: a
// single-value variable is inserted raw (so it can be used as an identifier
// or quoted by the query author), while multi-value selections become an
// escaped, quoted SQL list usable with IN (...). Use ${var:sqlstring} to
// force escaping of single values.
export const interpolateQueryExpr = (value: string | string[], variable: { multi?: boolean; includeAll?: boolean }) => {
  if (typeof value === 'string' && !variable.multi && !variable.includeAll) {
    return value;
  }
  if (Array.isArray(value)) {
    return value.map(quoteLiteral).join(',');
  }
  return quoteLiteral(value);
};
