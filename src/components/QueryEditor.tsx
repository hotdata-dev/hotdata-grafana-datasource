import React, { useEffect, useRef, useState } from 'react';
import { CodeEditor, Combobox, ComboboxOption, InlineField, InlineFieldRow, monacoTypes } from '@grafana/ui';
import { QueryEditorProps } from '@grafana/data';
import { DataSource } from '../datasource';
import { registerSqlCompletions } from '../completions';
import { DatabaseResource, Dialect, HotdataDataSourceOptions, HotdataQuery, QueryFormat } from '../types';

type Props = QueryEditorProps<DataSource, HotdataQuery, HotdataDataSourceOptions>;

const FORMATS: Array<ComboboxOption<QueryFormat>> = [
  { label: 'Table', value: 'table' },
  { label: 'Time series', value: 'timeseries' },
];

const DIALECTS: Array<ComboboxOption<Dialect>> = [
  { label: 'HotSQL (native)', value: 'hotsql' },
  { label: 'PostgreSQL', value: 'postgres' },
  { label: 'DuckDB', value: 'duckdb' },
  { label: 'Snowflake', value: 'snowflake' },
];

export function QueryEditor({ datasource, query, onChange, onRunQuery }: Props) {
  const [databases, setDatabases] = useState<Array<ComboboxOption<string>>>([]);
  const databaseIdRef = useRef(query.databaseId);
  const completionsRef = useRef<monacoTypes.IDisposable>();

  useEffect(() => {
    databaseIdRef.current = query.databaseId;
  }, [query.databaseId]);

  useEffect(() => () => completionsRef.current?.dispose(), []);

  useEffect(() => {
    let active = true;
    datasource
      .getResource<DatabaseResource[]>('databases')
      .then((dbs) => {
        if (active) {
          setDatabases(dbs.map((db) => ({ label: db.name, description: db.id, value: db.id })));
        }
      })
      .catch(() => {
        // Picker degrades to free-text entry when discovery fails.
      });
    return () => {
      active = false;
    };
  }, [datasource]);

  return (
    <>
      <InlineFieldRow>
        <InlineField
          label="Database"
          labelWidth={12}
          tooltip="Instant database to run in. Empty uses the datasource default."
        >
          <Combobox
            id="query-database"
            width={38}
            isClearable
            createCustomValue
            placeholder="Datasource default"
            options={databases}
            value={query.databaseId ?? null}
            onChange={(v) => onChange({ ...query, databaseId: v?.value })}
          />
        </InlineField>
        <InlineField label="Format" labelWidth={10}>
          <Combobox
            id="query-format"
            width={18}
            options={FORMATS}
            value={query.format || 'table'}
            onChange={(v) => {
              onChange({ ...query, format: v.value });
              onRunQuery();
            }}
          />
        </InlineField>
        <InlineField label="Dialect" labelWidth={10} tooltip="Empty uses the datasource default.">
          <Combobox
            id="query-dialect"
            width={22}
            isClearable
            placeholder="Datasource default"
            options={DIALECTS}
            value={query.dialect ?? null}
            onChange={(v) => onChange({ ...query, dialect: v?.value })}
          />
        </InlineField>
      </InlineFieldRow>
      <CodeEditor
        aria-label="SQL"
        language="sql"
        value={query.rawSql || ''}
        height="160px"
        showLineNumbers
        onEditorDidMount={(editor, monaco) => {
          // Dispose any prior registration before replacing it, so an in-place
          // editor remount doesn't orphan a completion provider.
          completionsRef.current?.dispose();
          completionsRef.current = registerSqlCompletions(monaco, editor, datasource, () => databaseIdRef.current);
        }}
        onBlur={(rawSql) => {
          onChange({ ...query, rawSql });
          onRunQuery();
        }}
        onSave={(rawSql) => {
          onChange({ ...query, rawSql });
          onRunQuery();
        }}
      />
    </>
  );
}
