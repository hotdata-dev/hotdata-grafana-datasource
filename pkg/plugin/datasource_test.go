package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/hotdata/hotdata/pkg/hotdata"
	"github.com/hotdata/hotdata/pkg/models"
)

// arrowFixture builds an Arrow IPC stream matching what
// GET /v1/results/{id}?format=arrow serves: Utf8View strings, Int64, Float64,
// Boolean, and a microsecond UTC timestamp, with a null mixed in.
func arrowFixture(t *testing.T) []byte {
	t.Helper()
	pool := memory.NewGoAllocator()
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}, Nullable: true},
		{Name: "n", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "f", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "s", Type: arrow.BinaryTypes.StringView, Nullable: true},
		{Name: "b", Type: arrow.FixedWidthTypes.Boolean, Nullable: true},
	}, nil)

	bld := array.NewRecordBuilder(pool, schema)
	defer bld.Release()

	base := time.Date(2026, 9, 4, 19, 55, 56, 0, time.UTC)
	tsBld := bld.Field(0).(*array.TimestampBuilder)
	tsBld.Append(arrow.Timestamp(base.UnixMicro()))
	tsBld.Append(arrow.Timestamp(base.Add(time.Minute).UnixMicro()))

	nBld := bld.Field(1).(*array.Int64Builder)
	nBld.Append(1)
	nBld.AppendNull()

	fBld := bld.Field(2).(*array.Float64Builder)
	fBld.Append(1.5)
	fBld.Append(2.5)

	sBld := bld.Field(3).(*array.StringViewBuilder)
	sBld.Append("x")
	sBld.Append("y")

	bBld := bld.Field(4).(*array.BooleanBuilder)
	bBld.Append(true)
	bBld.Append(false)

	rec := bld.NewRecordBatch()
	defer rec.Release()

	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(schema), ipc.WithAllocator(pool))
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type serverOpts struct {
	async           bool
	pollsNeeded     int32
	runFails        bool // the async run ends "failed" with an error_message
	tooLarge        bool // POST answers 202 + X-Result-Id: result too large to inline
	jsonOnly        bool // POST ignores the Arrow Accept header and answers JSON
	rateLimit       bool
	resultNotReady  int32         // fetches that return a 202 "processing" placeholder first
	postUnavailable bool          // POST /v1/query answers 503 (never retryable: could re-run SQL)
	postCount       *atomic.Int32 // when set, counts POST /v1/query requests
}

// newContractServer serves the captured Hotdata v1 wire contract.
func newContractServer(t *testing.T, opts serverOpts) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	arrowBody := arrowFixture(t)
	polls := &atomic.Int32{}
	rateLimited := &atomic.Bool{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/query", func(w http.ResponseWriter, r *http.Request) {
		if opts.postCount != nil {
			opts.postCount.Add(1)
		}
		if opts.postUnavailable {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"WORKER_UNAVAILABLE"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-workspace-id") != "work123" || r.Header.Get("x-database-id") != "dbid123" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if opts.rateLimit && rateLimited.CompareAndSwap(false, true) {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"OVERLOADED"}`))
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["sql"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Production contract: async_after_ms has a minimum of 1000.
		if ms, ok := body["async_after_ms"].(float64); !ok || ms < 1000 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"async_after_ms must be at least 1000","code":"BAD_REQUEST"}}`))
			return
		}
		if opts.tooLarge {
			w.Header().Set("X-Result-Id", "rslt1")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"query_run_id":"qrun1","status":"running","status_url":"/v1/query-runs/qrun1"}`))
			return
		}
		if opts.async {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"query_run_id":"qrun1","status":"running","status_url":"/v1/query-runs/qrun1"}`))
			return
		}
		if opts.jsonOnly || r.Header.Get("Accept") != "application/vnd.apache.arrow.stream" {
			_, _ = w.Write([]byte(`{"query_run_id":"qrun1","result_id":"rslt1","columns":["ts","n","f","s","b"],"rows":[],"preview_row_count":2,"total_row_count":2,"truncated":false,"execution_time_ms":2}`))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apache.arrow.stream")
		w.Header().Set("X-Query-Run-Id", "qrun1")
		w.Header().Set("X-Result-Id", "rslt1")
		w.Header().Set("X-Total-Row-Count", "2")
		w.Header().Set("X-Execution-Time-Ms", "2")
		_, _ = w.Write(arrowBody)
	})
	// Production contract: query-runs and results are database-scoped.
	requireDB := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("x-database-id") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"X-Database-Id header is required: this endpoint is scoped to a database","code":"BAD_REQUEST"}}`))
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/query-runs/qrun1", func(w http.ResponseWriter, r *http.Request) {
		if !requireDB(w, r) {
			return
		}
		if polls.Add(1) < opts.pollsNeeded {
			_, _ = w.Write([]byte(`{"id":"qrun1","status":"running"}`))
			return
		}
		if opts.runFails {
			_, _ = w.Write([]byte(`{"id":"qrun1","status":"failed","error_message":"table 'default.public.t' not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"qrun1","status":"succeeded","result_id":"rslt1","row_count":2,"execution_time_ms":1500}`))
	})
	resultFetches := &atomic.Int32{}
	mux.HandleFunc("GET /v1/results/rslt1", func(w http.ResponseWriter, r *http.Request) {
		if !requireDB(w, r) {
			return
		}
		if r.URL.Query().Get("format") != "arrow" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// The result is not persisted yet: 202 with a JSON placeholder before
		// the Arrow stream is served. Production sends Retry-After: 1, which
		// the client deliberately ignores in favor of its faster backoff.
		if resultFetches.Add(1) <= opts.resultNotReady {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"result_id":"rslt1","status":"processing"}`))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apache.arrow.stream")
		_, _ = w.Write(arrowBody)
	})
	// search is a case-insensitive name substring filter; limit sizes the page.
	mux.HandleFunc("GET /v1/databases", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch strings.ToLower(r.URL.Query().Get("search")) {
		case "":
			_, _ = w.Write([]byte(`{"databases":[{"id":"dbid123","name":"test","default_catalog":"default","default_schema":"public"},{"id":"dbid456","name":"other","default_catalog":"default","default_schema":"main"}],"count":2,"has_more":true,"next_cursor":"p2"}`))
		case "oth":
			_, _ = w.Write([]byte(`{"databases":[{"id":"dbid456","name":"other","default_catalog":"default","default_schema":"main"}],"count":1,"has_more":false}`))
		default:
			_, _ = w.Write([]byte(`{"databases":[],"count":0,"has_more":false}`))
		}
	})
	mux.HandleFunc("GET /v1/databases/dbid123", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"dbid123","name":"test","default_connection_id":"conn123","default_catalog":"default","default_schema":"public"}`))
	})
	mux.HandleFunc("GET /v1/information_schema", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("connection_id") != "conn123" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		cols := ""
		if r.URL.Query().Get("include_columns") == "true" {
			cols = `,"columns":[{"name":"city","data_type":"Utf8View","nullable":true}]`
		}
		if r.URL.Query().Get("table") == "" && r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"tables":[{"schema":"cities","table":"global","synced":true` + cols + `}],"count":1,"has_more":true,"next_cursor":"p2"}`))
			return
		}
		if r.URL.Query().Get("cursor") == "p2" {
			_, _ = w.Write([]byte(`{"tables":[{"schema":"sales","table":"orders","synced":true}],"count":1,"has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"tables":[{"schema":"cities","table":"global","synced":true` + cols + `}],"count":1,"has_more":false}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, polls
}

func newTestDatasource(srv *httptest.Server) *Datasource {
	client := hotdata.New(srv.URL, "test-key", "work123", srv.Client())
	client.PollInitial = time.Millisecond
	client.PollMax = 2 * time.Millisecond
	return &Datasource{
		settings: &models.PluginSettings{
			ApiUrl:            srv.URL,
			WorkspaceID:       "work123",
			DefaultDatabaseID: "dbid123",
			Secrets:           &models.SecretPluginSettings{ApiKey: "test-key"},
		},
		client: client,
	}
}

func runQuery(t *testing.T, ds *Datasource, queryJSON string) backend.DataResponse {
	t.Helper()
	resp, err := ds.QueryData(context.Background(), &backend.QueryDataRequest{
		Queries: []backend.DataQuery{{RefID: "A", JSON: []byte(queryJSON), TimeRange: backend.TimeRange{
			From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			To:   time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Responses["A"]
}

// A 503 on POST /v1/query must never be retried: the response may have been
// lost after the SQL executed, so a retry could run the statement twice.
func TestNoRetryOnQueryPostUnavailable(t *testing.T) {
	posts := &atomic.Int32{}
	srv, _ := newContractServer(t, serverOpts{postUnavailable: true, postCount: posts})
	ds := newTestDatasource(srv)

	res := runQuery(t, ds, `{"rawSql":"SELECT 1"}`)
	if res.Error == nil {
		t.Fatal("expected an error when POST /v1/query answers 503")
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("POST /v1/query sent %d times, want exactly 1 (no retry on 503)", got)
	}
}

func assertFixtureFrame(t *testing.T, res backend.DataResponse) {
	t.Helper()
	if res.Error != nil {
		t.Fatalf("unexpected query error: %v", res.Error)
	}
	if len(res.Frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(res.Frames))
	}
	frame := res.Frames[0]
	if len(frame.Fields) != 5 {
		t.Fatalf("expected 5 fields, got %d", len(frame.Fields))
	}
	if frame.Rows() != 2 {
		t.Fatalf("expected 2 rows, got %d", frame.Rows())
	}

	ts := frame.Fields[0].At(0).(*time.Time)
	want := time.Date(2026, 9, 4, 19, 55, 56, 0, time.UTC)
	if !ts.Equal(want) {
		t.Errorf("ts = %v, want %v", ts, want)
	}
	if n := frame.Fields[1].At(0).(*int64); *n != 1 {
		t.Errorf("n = %d, want 1", *n)
	}
	if frame.Fields[1].At(1) != nil {
		if v := frame.Fields[1].At(1).(*int64); v != nil {
			t.Errorf("expected null n at row 1, got %d", *v)
		}
	}
	if f := frame.Fields[2].At(1).(*float64); *f != 2.5 {
		t.Errorf("f = %f, want 2.5", *f)
	}
	if s := frame.Fields[3].At(0).(*string); *s != "x" {
		t.Errorf("s = %q, want x", *s)
	}
	if b := frame.Fields[4].At(0).(*bool); !*b {
		t.Error("b = false, want true")
	}
}

func TestQueryDataSyncFlow(t *testing.T) {
	srv, polls := newContractServer(t, serverOpts{})
	ds := newTestDatasource(srv)

	res := runQuery(t, ds, `{"rawSql":"SELECT * FROM t","format":"table"}`)
	assertFixtureFrame(t, res)
	if polls.Load() != 0 {
		t.Errorf("sync flow should not poll, polled %d times", polls.Load())
	}
	if res.Frames[0].Meta.ExecutedQueryString != "SELECT * FROM t" {
		t.Errorf("executed query = %q", res.Frames[0].Meta.ExecutedQueryString)
	}
}

func TestQueryDataAsyncFlow(t *testing.T) {
	srv, polls := newContractServer(t, serverOpts{async: true, pollsNeeded: 3})
	ds := newTestDatasource(srv)

	res := runQuery(t, ds, `{"rawSql":"SELECT * FROM t","format":"table"}`)
	assertFixtureFrame(t, res)
	if polls.Load() != 3 {
		t.Errorf("expected 3 polls, got %d", polls.Load())
	}
}

func TestQueryDataRetriesOn429(t *testing.T) {
	srv, _ := newContractServer(t, serverOpts{rateLimit: true})
	ds := newTestDatasource(srv)

	res := runQuery(t, ds, `{"rawSql":"SELECT * FROM t","format":"table"}`)
	assertFixtureFrame(t, res)
}

// A server that answers JSON despite the Arrow Accept header falls back to
// fetching the result, polling through 202 "processing" placeholders until
// the Arrow stream is ready.
func TestQueryDataJSONFallback(t *testing.T) {
	srv, _ := newContractServer(t, serverOpts{jsonOnly: true, resultNotReady: 2})
	ds := newTestDatasource(srv)

	res := runQuery(t, ds, `{"rawSql":"SELECT * FROM t","format":"table"}`)
	assertFixtureFrame(t, res)
}

// A result too large to inline answers 202 with X-Result-Id; the client reads
// it from the Results API without polling the run.
func TestQueryDataTooLarge(t *testing.T) {
	srv, polls := newContractServer(t, serverOpts{tooLarge: true, resultNotReady: 1})
	ds := newTestDatasource(srv)

	res := runQuery(t, ds, `{"rawSql":"SELECT * FROM t","format":"table"}`)
	assertFixtureFrame(t, res)
	if polls.Load() != 0 {
		t.Errorf("too-large flow should not poll the run, polled %d times", polls.Load())
	}
}

// A failed async run surfaces the server's error_message.
func TestQueryDataRunFailed(t *testing.T) {
	srv, _ := newContractServer(t, serverOpts{async: true, runFails: true})
	ds := newTestDatasource(srv)

	res := runQuery(t, ds, `{"rawSql":"SELECT * FROM t","format":"table"}`)
	if res.Error == nil || !strings.Contains(res.Error.Error(), "table 'default.public.t' not found") {
		t.Fatalf("expected the run's error_message, got %v", res.Error)
	}
}

func TestAPIErrorMessage(t *testing.T) {
	for body, want := range map[string]string{
		`{"error":{"code":"NOT_FOUND","message":"Result 'x' not found"}}`: "Result 'x' not found (NOT_FOUND)",
		`{"error":"missing_authorization"}`:                               "missing_authorization",
		`{"status":"failed","error_message":"out of memory"}`:             "out of memory",
		`upstream timeout`: "upstream timeout",
	} {
		err := &hotdata.APIError{StatusCode: 400, Body: body}
		if got := err.Error(); got != "hotdata API: HTTP 400: "+want {
			t.Errorf("body %s: got %q, want message %q", body, got, want)
		}
	}
}

func TestQueryDataNoDatabase(t *testing.T) {
	srv, _ := newContractServer(t, serverOpts{})
	ds := newTestDatasource(srv)
	ds.settings.DefaultDatabaseID = ""

	res := runQuery(t, ds, `{"rawSql":"SELECT 1","format":"table"}`)
	if res.Error == nil || !strings.Contains(res.Error.Error(), "no database selected") {
		t.Fatalf("expected no-database error, got %v", res.Error)
	}
}

func TestMacros(t *testing.T) {
	tr := backend.TimeRange{
		From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 9, 4, 12, 30, 0, 0, time.UTC),
	}
	got := interpolateMacros(`SELECT * FROM t WHERE $__timeFilter(created_at) AND x > $__timeFrom()`, tr, time.Minute)
	want := `SELECT * FROM t WHERE (created_at >= '2026-09-01T00:00:00Z' AND created_at <= '2026-09-04T12:30:00Z') AND x > '2026-09-01T00:00:00Z'`
	if got != want {
		t.Errorf("macros:\n got  %s\n want %s", got, want)
	}
}

func TestTimeGroupMacro(t *testing.T) {
	tr := backend.TimeRange{
		From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
	}
	cases := []struct{ in, want string }{
		{
			`SELECT $__timeGroup(ts, 5m) AS bucket`,
			`SELECT date_bin(interval '300000 millisecond', ts, timestamp '1970-01-01T00:00:00Z') AS bucket`,
		},
		{
			`SELECT $__timeGroup(ts, '1h')`,
			`SELECT date_bin(interval '3600000 millisecond', ts, timestamp '1970-01-01T00:00:00Z')`,
		},
		{
			// $__interval (30s here) composes inside $__timeGroup.
			`SELECT $__timeGroup(ts, $__interval)`,
			`SELECT date_bin(interval '30000 millisecond', ts, timestamp '1970-01-01T00:00:00Z')`,
		},
		{
			`SELECT $__interval_ms`,
			`SELECT 30000`,
		},
	}
	for _, c := range cases {
		if got := interpolateMacros(c.in, tr, 30*time.Second); got != c.want {
			t.Errorf("timeGroup:\n got  %s\n want %s", got, c.want)
		}
	}
}

// Column expressions may contain commas and parentheses; balanced-paren
// scanning must not truncate them at the first inner comma/paren.
func TestMacrosNestedArgs(t *testing.T) {
	tr := backend.TimeRange{
		From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
	}
	cases := []struct{ in, want string }{
		{
			`SELECT $__timeGroup(date_trunc('hour', ts), 5m)`,
			`SELECT date_bin(interval '300000 millisecond', date_trunc('hour', ts), timestamp '1970-01-01T00:00:00Z')`,
		},
		{
			`WHERE $__timeFilter(coalesce(a, b))`,
			`WHERE (coalesce(a, b) >= '2026-09-01T00:00:00Z' AND coalesce(a, b) <= '2026-09-04T00:00:00Z')`,
		},
	}
	for _, c := range cases {
		if got := interpolateMacros(c.in, tr, 5*time.Minute); got != c.want {
			t.Errorf("nested args:\n got  %s\n want %s", got, c.want)
		}
	}
}

type resourceRecorder struct {
	status int
	body   []byte
}

func (r *resourceRecorder) Send(res *backend.CallResourceResponse) error {
	r.status = res.Status
	r.body = res.Body
	return nil
}

func TestResources(t *testing.T) {
	srv, _ := newContractServer(t, serverOpts{})
	ds := newTestDatasource(srv)

	t.Run("tables", func(t *testing.T) {
		rec := &resourceRecorder{}
		err := ds.CallResource(context.Background(), &backend.CallResourceRequest{Path: "tables", URL: "tables?database=dbid123"}, rec)
		if err != nil || rec.status != 200 {
			t.Fatalf("tables: err=%v status=%d body=%s", err, rec.status, rec.body)
		}
		var tables []hotdata.Table
		if err := json.Unmarshal(rec.body, &tables); err != nil {
			t.Fatal(err)
		}
		if len(tables) != 2 || tables[0].Table != "global" || tables[1].Table != "orders" {
			t.Fatalf("expected both pages of tables, got %+v", tables)
		}
	})

	databases := func(t *testing.T, url string) []hotdata.Database {
		t.Helper()
		rec := &resourceRecorder{}
		err := ds.CallResource(context.Background(), &backend.CallResourceRequest{Path: "databases", URL: url}, rec)
		if err != nil || rec.status != 200 {
			t.Fatalf("%s: err=%v status=%d body=%s", url, err, rec.status, rec.body)
		}
		var dbs []hotdata.Database
		if err := json.Unmarshal(rec.body, &dbs); err != nil {
			t.Fatal(err)
		}
		return dbs
	}

	t.Run("databases", func(t *testing.T) {
		if dbs := databases(t, "databases"); len(dbs) != 2 {
			t.Fatalf("expected one page of 2 databases, got %+v", dbs)
		}
	})

	t.Run("databases search", func(t *testing.T) {
		if dbs := databases(t, "databases?search=Oth"); len(dbs) != 1 || dbs[0].ID != "dbid456" {
			t.Fatalf("expected the matching database, got %+v", dbs)
		}
		// No match must encode as [], not null, for the frontend picker.
		rec := &resourceRecorder{}
		_ = ds.CallResource(context.Background(), &backend.CallResourceRequest{Path: "databases", URL: "databases?search=zzz"}, rec)
		if string(rec.body) != "[]" {
			t.Fatalf("expected [], got %s", rec.body)
		}
	})

	t.Run("databases by id", func(t *testing.T) {
		if dbs := databases(t, "databases?id=dbid123"); len(dbs) != 1 || dbs[0].Name != "test" {
			t.Fatalf("expected database dbid123, got %+v", dbs)
		}
	})

	t.Run("columns", func(t *testing.T) {
		rec := &resourceRecorder{}
		err := ds.CallResource(context.Background(), &backend.CallResourceRequest{Path: "columns", URL: "columns?database=dbid123&schema=cities&table=global"}, rec)
		if err != nil || rec.status != 200 {
			t.Fatalf("columns: err=%v status=%d body=%s", err, rec.status, rec.body)
		}
		var cols []hotdata.Column
		if err := json.Unmarshal(rec.body, &cols); err != nil {
			t.Fatal(err)
		}
		if len(cols) != 1 || cols[0].Name != "city" || cols[0].DataType != "Utf8View" {
			t.Fatalf("unexpected columns: %+v", cols)
		}
	})

	t.Run("schemas", func(t *testing.T) {
		rec := &resourceRecorder{}
		err := ds.CallResource(context.Background(), &backend.CallResourceRequest{Path: "schemas", URL: "schemas?database=dbid123"}, rec)
		if err != nil || rec.status != 200 {
			t.Fatalf("schemas: err=%v status=%d body=%s", err, rec.status, rec.body)
		}
		if string(rec.body) != `["cities","sales"]` {
			t.Fatalf("unexpected schemas: %s", rec.body)
		}
	})
}

func TestCheckHealth(t *testing.T) {
	srv, _ := newContractServer(t, serverOpts{})
	ds := newTestDatasource(srv)

	res, err := ds.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != backend.HealthStatusOk {
		t.Fatalf("health = %s: %s", res.Status, res.Message)
	}
}

func TestCheckHealthMissingKey(t *testing.T) {
	srv, _ := newContractServer(t, serverOpts{})
	ds := newTestDatasource(srv)
	ds.settings.Secrets.ApiKey = ""

	res, err := ds.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != backend.HealthStatusError || !strings.Contains(res.Message, "API key") {
		t.Fatalf("expected API key error, got %s: %s", res.Status, res.Message)
	}
}
