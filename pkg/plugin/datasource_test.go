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
	async          bool
	pollsNeeded    int32
	rateLimit      bool
	resultNotReady int32 // fetches that return a JSON "processing" placeholder first
}

// newContractServer serves the captured Hotdata v1 wire contract.
func newContractServer(t *testing.T, opts serverOpts) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	arrowBody := arrowFixture(t)
	polls := &atomic.Int32{}
	rateLimited := &atomic.Bool{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/query", func(w http.ResponseWriter, r *http.Request) {
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
		if opts.async {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"query_run_id":"qrun1","status":"running","status_url":"/v1/query-runs/qrun1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"query_run_id":"qrun1","result_id":"rslt1","columns":["ts","n","f","s","b"],"rows":[],"row_count":2,"truncated":false,"execution_time_ms":2}`))
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
		// Model the fast-path race: the result is not materialized yet, so the
		// endpoint answers 200 with a JSON placeholder before the Arrow stream.
		if resultFetches.Add(1) <= opts.resultNotReady {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result_id":"rslt1","status":"processing"}`))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apache.arrow.stream")
		_, _ = w.Write(arrowBody)
	})
	mux.HandleFunc("GET /v1/databases", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"databases":[{"id":"dbid123","name":"test","default_catalog":"default","default_schema":"public"}]}`))
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
		_, _ = w.Write([]byte(`{"tables":[{"schema":"cities","table":"global","synced":true` + cols + `}]}`))
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

// The fast/sync path can fetch the result before it is materialized; the
// endpoint returns a JSON "processing" placeholder until the Arrow stream is
// ready. The client must poll through it.
func TestQueryDataResultNotReady(t *testing.T) {
	srv, _ := newContractServer(t, serverOpts{resultNotReady: 2})
	ds := newTestDatasource(srv)

	res := runQuery(t, ds, `{"rawSql":"SELECT * FROM t","format":"table"}`)
	assertFixtureFrame(t, res)
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
		if len(tables) != 1 || tables[0].Schema != "cities" || tables[0].Table != "global" {
			t.Fatalf("unexpected tables: %+v", tables)
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
		if string(rec.body) != `["cities"]` {
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
