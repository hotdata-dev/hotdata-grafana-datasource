import React, { useCallback, useEffect, useRef, useState } from 'react';
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

const toOption = (db: DatabaseResource): ComboboxOption<string> => ({
  label: db.name || db.id,
  description: db.id,
  value: db.id,
});

export function QueryEditor({ datasource, query, onChange, onRunQuery }: Props) {
  // Label for the selected database. Options load per search, so the saved id
  // is resolved separately to show its name rather than the raw id.
  const [selected, setSelected] = useState<ComboboxOption<string> | null>(null);
  const databaseIdRef = useRef(query.databaseId);
  const completionsRef = useRef<monacoTypes.IDisposable>();

  useEffect(() => {
    databaseIdRef.current = query.databaseId;
  }, [query.databaseId]);

  useEffect(() => () => completionsRef.current?.dispose(), []);

  // Workspaces can hold thousands of databases, so the picker searches by
  // name on the server instead of loading them all.
  const loadDatabases = useCallback(
    (search: string) =>
      datasource
        .getResource<DatabaseResource[]>('databases', search ? { search } : undefined)
        .then((dbs) => dbs.map(toOption))
        // Picker degrades to free-text entry when discovery fails.
        .catch(() => []),
    [datasource]
  );

  useEffect(() => {
    const id = query.databaseId;
    if (!id) {
      return;
    }
    let active = true;
    datasource
      .getResource<DatabaseResource[]>('databases', { id })
      .then((dbs) => {
        if (active && dbs[0]) {
          setSelected(toOption(dbs[0]));
        }
      })
      .catch(() => {
        // Falls back to showing the raw id.
      });
    return () => {
      active = false;
    };
  }, [datasource, query.databaseId]);

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
            options={loadDatabases}
            value={selected?.value === query.databaseId ? selected : (query.databaseId ?? null)}
            onChange={(v) => {
              setSelected(v);
              onChange({ ...query, databaseId: v?.value });
            }}
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
