import type { Monaco, monacoTypes } from '@grafana/ui';
import { DataSource } from './datasource';

interface TableInfo {
  schema: string;
  table: string;
}

interface ColumnInfo {
  name: string;
  data_type: string;
  nullable: boolean;
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

const MACROS: Array<{ label: string; insert: string; detail: string }> = [
  { label: '$__timeFilter(column)', insert: '\\$__timeFilter(${1:column})', detail: 'column between dashboard from/to' },
  { label: '$__timeFrom()', insert: '\\$__timeFrom()', detail: 'dashboard range start (UTC literal)' },
  { label: '$__timeTo()', insert: '\\$__timeTo()', detail: 'dashboard range end (UTC literal)' },
  { label: '$__timeGroup(column, interval)', insert: '\\$__timeGroup(${1:column}, ${2:$__interval})', detail: 'time bucket via date_bin' },
  { label: '$__interval', insert: '\\$__interval', detail: 'panel interval (e.g. 30s)' },
  { label: '$__interval_ms', insert: '\\$__interval_ms', detail: 'panel interval in milliseconds' },
];

/**
 * Registers a SQL completion provider offering macros, tables from the
 * active database, and columns for tables referenced in the query. Column
 * lookups go through the plugin backend, so the API key stays server-side.
 */
export function registerSqlCompletions(
  monaco: Monaco,
  editor: monacoTypes.editor.IStandaloneCodeEditor,
  datasource: DataSource,
  getDatabaseId: () => string | undefined
): monacoTypes.IDisposable {
  const tableCache = new Map<string, Promise<TableInfo[]>>();
  const columnCache = new Map<string, Promise<ColumnInfo[]>>();

  // Cache successes only: on failure, drop the entry so a transient blip
  // doesn't permanently pin an empty result for the editor's lifetime.
  const fetchTables = (db: string) => {
    if (!tableCache.has(db)) {
      const p = datasource
        .getResource<TableInfo[]>('tables', db ? { database: db } : undefined)
        .catch(() => {
          tableCache.delete(db);
          return [] as TableInfo[];
        });
      tableCache.set(db, p);
    }
    return tableCache.get(db)!;
  };

  const fetchColumns = (db: string, schema: string, table: string) => {
    const key = `${db}|${schema}.${table}`;
    if (!columnCache.has(key)) {
      const p = datasource
        .getResource<ColumnInfo[]>('columns', { database: db, schema, table })
        .catch(() => {
          columnCache.delete(key);
          return [] as ColumnInfo[];
        });
      columnCache.set(key, p);
    }
    return columnCache.get(key)!;
  };

  return monaco.languages.registerCompletionItemProvider('sql', {
    triggerCharacters: ['$', '.', ' '],
    provideCompletionItems: async (model, position) => {
      // The provider is language-global; only serve our own editor instance.
      if (model !== editor.getModel()) {
        return { suggestions: [] };
      }

      const word = model.getWordUntilPosition(position);
      const range = {
        startLineNumber: position.lineNumber,
        endLineNumber: position.lineNumber,
        startColumn: word.startColumn,
        endColumn: word.endColumn,
      };

      const suggestions: monacoTypes.languages.CompletionItem[] = MACROS.map((m) => ({
        label: m.label,
        detail: m.detail,
        kind: monaco.languages.CompletionItemKind.Function,
        insertText: m.insert,
        insertTextRules: monaco.languages.CompletionItemInsertTextRule.InsertAsSnippet,
        range,
      }));

      const db = getDatabaseId() ?? '';
      const tables = await fetchTables(db);

      for (const t of tables) {
        suggestions.push({
          label: `${t.schema}.${t.table}`,
          detail: 'table',
          kind: monaco.languages.CompletionItemKind.Class,
          insertText: `${t.schema}.${t.table}`,
          range,
        });
      }

      // Columns for tables already referenced in the query. Match on word
      // boundaries so short names (t, id, log) don't match arbitrary substrings.
      const text = model.getValue();
      const mentions = (name: string) => new RegExp(`(^|[^\\w.])${escapeRegExp(name)}(\\b|$)`).test(text);
      const referenced = tables.filter((t) => text.includes(`${t.schema}.${t.table}`) || mentions(t.table));
      const columns = await Promise.all(referenced.slice(0, 5).map((t) => fetchColumns(db, t.schema, t.table)));
      for (const cols of columns) {
        for (const c of cols) {
          suggestions.push({
            label: c.name,
            detail: c.data_type,
            kind: monaco.languages.CompletionItemKind.Field,
            insertText: c.name,
            range,
          });
        }
      }

      return { suggestions };
    },
  });
}
