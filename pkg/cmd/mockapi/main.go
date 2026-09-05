// Command mockapi serves a local stand-in for the Hotdata v1 API implementing
// the wire contract the plugin depends on (see SPEC.md §3). Used for e2e tests
// and for developing the plugin without a live workspace:
//
//	go run ./pkg/cmd/mockapi            # listens on :8999
//
// SQL containing the word "slow" exercises the async 202 + poll flow.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func main() {
	addr := flag.String("addr", ":8999", "listen address")
	flag.Parse()

	var polls atomic.Int32

	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/query", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SQL string `json:"sql"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		log.Printf("query db=%s sql=%q", r.Header.Get("x-database-id"), body.SQL)

		if bytes.Contains([]byte(body.SQL), []byte("slow")) {
			polls.Store(0)
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprint(w, `{"query_run_id":"qrunmock","status":"running","status_url":"/v1/query-runs/qrunmock"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"query_run_id":"qrunmock","result_id":"rsltmock","columns":["ts","service","value"],"rows":[],"row_count":180,"truncated":false,"execution_time_ms":3}`)
	})

	// Production contract: query-runs and results are database-scoped.
	requireDB := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("x-database-id") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"X-Database-Id header is required: this endpoint is scoped to a database","code":"BAD_REQUEST"}}`)
			return false
		}
		return true
	}

	mux.HandleFunc("GET /v1/query-runs/qrunmock", func(w http.ResponseWriter, r *http.Request) {
		if !requireDB(w, r) {
			return
		}
		if polls.Add(1) < 3 {
			_, _ = fmt.Fprint(w, `{"id":"qrunmock","status":"running"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"qrunmock","status":"succeeded","result_id":"rsltmock","row_count":180,"execution_time_ms":2100}`)
	})

	var resultFetches atomic.Int32
	mux.HandleFunc("GET /v1/results/rsltmock", func(w http.ResponseWriter, r *http.Request) {
		if !requireDB(w, r) {
			return
		}
		// Model the fast-path race: first fetch returns a JSON placeholder
		// before the Arrow stream is materialized.
		if resultFetches.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"result_id":"rsltmock","status":"processing"}`)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apache.arrow.stream")
		if err := writeSeries(w); err != nil {
			log.Printf("writing arrow stream: %v", err)
		}
	})

	mux.HandleFunc("GET /v1/databases", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"databases":[{"id":"dbmock","name":"mock-db","default_catalog":"default","default_schema":"public","created_at":"2026-09-04T00:00:00Z"}]}`)
	})

	mux.HandleFunc("GET /v1/databases/dbmock", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"dbmock","name":"mock-db","default_connection_id":"connmock","default_catalog":"default","default_schema":"public","attachments":[],"created_at":"2026-09-04T00:00:00Z"}`)
	})

	mux.HandleFunc("GET /v1/information_schema", func(w http.ResponseWriter, r *http.Request) {
		cols := ""
		if r.URL.Query().Get("include_columns") == "true" {
			cols = `,"columns":[{"name":"ts","data_type":"Timestamp(Microsecond, Some(\"UTC\"))","nullable":true},{"name":"service","data_type":"Utf8View","nullable":true},{"name":"value","data_type":"Float64","nullable":true}]`
		}
		_, _ = fmt.Fprintf(w, `{"tables":[{"connection":"__db_mock","schema":"public","table":"metrics","synced":true,"last_sync":"2026-09-04 00:00:00"%s,"partition_by":[],"sorted_by":[]}]}`, cols)
	})

	mux.HandleFunc("GET /v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"workspaces":[{"public_id":"workmock","name":"mock-workspace","active":true}]}`)
	})

	log.Printf("hotdata mock API listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// writeSeries emits a long-format time series: 90 minutes × 2 services, one
// row per minute per service — enough for LongToWide and panel rendering.
func writeSeries(w http.ResponseWriter) error {
	pool := memory.NewGoAllocator()
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}, Nullable: true},
		{Name: "service", Type: arrow.BinaryTypes.StringView, Nullable: true},
		{Name: "value", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	}, nil)

	bld := array.NewRecordBuilder(pool, schema)
	defer bld.Release()
	tsB := bld.Field(0).(*array.TimestampBuilder)
	svcB := bld.Field(1).(*array.StringViewBuilder)
	valB := bld.Field(2).(*array.Float64Builder)

	start := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Minute)
	for i := 0; i < 90; i++ {
		t := start.Add(time.Duration(i) * time.Minute)
		for s, svc := range []string{"api", "worker"} {
			tsB.Append(arrow.Timestamp(t.UnixMicro()))
			svcB.Append(svc)
			valB.Append(50 + 40*math.Sin(float64(i)/9+float64(s)*2) + float64(i%7))
		}
	}

	rec := bld.NewRecordBatch()
	defer rec.Release()

	ipcW := ipc.NewWriter(w, ipc.WithSchema(schema), ipc.WithAllocator(pool))
	if err := ipcW.Write(rec); err != nil {
		return err
	}
	return ipcW.Close()
}
