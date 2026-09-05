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
      rawSql: getTemplateSrv().replace(query.rawSql, scopedVars),
    };
  }

  filterQuery(query: HotdataQuery): boolean {
    return !!query.rawSql?.trim();
  }
}
