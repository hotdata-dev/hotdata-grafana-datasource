import { DataSourcePlugin } from '@grafana/data';
import { DataSource } from './datasource';
import { ConfigEditor } from './components/ConfigEditor';
import { QueryEditor } from './components/QueryEditor';
import { HotdataDataSourceOptions, HotdataQuery } from './types';

export const plugin = new DataSourcePlugin<DataSource, HotdataQuery, HotdataDataSourceOptions>(DataSource)
  .setConfigEditor(ConfigEditor)
  .setQueryEditor(QueryEditor);
