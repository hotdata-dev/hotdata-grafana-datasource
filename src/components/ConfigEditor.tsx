import React, { ChangeEvent } from 'react';
import { Combobox, ComboboxOption, FieldSet, InlineField, Input, SecretInput } from '@grafana/ui';
import { DataSourcePluginOptionsEditorProps } from '@grafana/data';
import { Dialect, HotdataDataSourceOptions, HotdataSecureJsonData } from '../types';

interface Props extends DataSourcePluginOptionsEditorProps<HotdataDataSourceOptions, HotdataSecureJsonData> {}

const DIALECTS: Array<ComboboxOption<Dialect>> = [
  { label: 'HotSQL (native)', value: 'hotsql' },
  { label: 'PostgreSQL', value: 'postgres' },
  { label: 'DuckDB', value: 'duckdb' },
  { label: 'Snowflake', value: 'snowflake' },
];

const LABEL_WIDTH = 22;
const FIELD_WIDTH = 44;

export function ConfigEditor(props: Props) {
  const { onOptionsChange, options } = props;
  const { jsonData, secureJsonFields, secureJsonData } = options;

  const onJsonDataChange =
    (key: keyof HotdataDataSourceOptions) => (event: ChangeEvent<HTMLInputElement>) => {
      onOptionsChange({
        ...options,
        jsonData: { ...jsonData, [key]: event.target.value.trim() },
      });
    };

  const onApiKeyChange = (event: ChangeEvent<HTMLInputElement>) => {
    onOptionsChange({
      ...options,
      secureJsonData: { apiKey: event.target.value },
    });
  };

  const onResetApiKey = () => {
    onOptionsChange({
      ...options,
      secureJsonFields: { ...options.secureJsonFields, apiKey: false },
      secureJsonData: { ...options.secureJsonData, apiKey: '' },
    });
  };

  return (
    <>
      <FieldSet label="Connection">
        <InlineField label="API URL" labelWidth={LABEL_WIDTH} tooltip="Hotdata API base URL.">
          <Input
            id="config-api-url"
            onChange={onJsonDataChange('apiUrl')}
            value={jsonData.apiUrl || ''}
            placeholder="https://api.hotdata.dev"
            width={FIELD_WIDTH}
          />
        </InlineField>
        <InlineField
          label="API key"
          labelWidth={LABEL_WIDTH}
          interactive
          tooltip="Workspace API key (hd_…). Stored encrypted and only used by the plugin backend."
        >
          <SecretInput
            required
            id="config-api-key"
            isConfigured={secureJsonFields.apiKey}
            value={secureJsonData?.apiKey}
            placeholder="hd_…"
            width={FIELD_WIDTH}
            onReset={onResetApiKey}
            onChange={onApiKeyChange}
          />
        </InlineField>
        <InlineField
          label="Workspace ID"
          labelWidth={LABEL_WIDTH}
          tooltip="Workspace public ID (work…). Find it with `hotdata workspaces list`."
        >
          <Input
            id="config-workspace-id"
            required
            onChange={onJsonDataChange('workspaceId')}
            value={jsonData.workspaceId || ''}
            placeholder="work…"
            width={FIELD_WIDTH}
          />
        </InlineField>
      </FieldSet>

      <FieldSet label="Defaults">
        <InlineField
          label="Default database ID"
          labelWidth={LABEL_WIDTH}
          tooltip="Instant database (dbid…) used when a query does not pick one. Also used by the health-check test query."
        >
          <Input
            id="config-default-database"
            onChange={onJsonDataChange('defaultDatabaseId')}
            value={jsonData.defaultDatabaseId || ''}
            placeholder="dbid…"
            width={FIELD_WIDTH}
          />
        </InlineField>
        <InlineField
          label="Default SQL dialect"
          labelWidth={LABEL_WIDTH}
          tooltip="Non-HotSQL dialects are transpiled server-side and are read-only."
        >
          <Combobox
            id="config-default-dialect"
            options={DIALECTS}
            value={jsonData.defaultDialect || 'postgres'}
            onChange={(v) => onOptionsChange({ ...options, jsonData: { ...jsonData, defaultDialect: v.value } })}
            width={FIELD_WIDTH}
          />
        </InlineField>
      </FieldSet>
    </>
  );
}
