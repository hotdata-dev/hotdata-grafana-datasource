package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/hotdata/hotdata/pkg/hotdata"
	"github.com/hotdata/hotdata/pkg/models"
)

var (
	_ backend.QueryDataHandler      = (*Datasource)(nil)
	_ backend.CheckHealthHandler    = (*Datasource)(nil)
	_ backend.CallResourceHandler   = (*Datasource)(nil)
	_ instancemgmt.InstanceDisposer = (*Datasource)(nil)
)

type Datasource struct {
	settings *models.PluginSettings
	client   *hotdata.Client

	// database id → default_connection_id, resolved lazily for the
	// information_schema resources.
	connMu    sync.Mutex
	connCache map[string]connEntry
}

type connEntry struct {
	id      string
	expires time.Time
}

func (d *Datasource) connectionID(ctx context.Context, databaseID string) (string, error) {
	d.connMu.Lock()
	entry, ok := d.connCache[databaseID]
	d.connMu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.id, nil
	}

	db, err := d.client.GetDatabase(ctx, databaseID)
	if err != nil {
		return "", err
	}
	d.connMu.Lock()
	if d.connCache == nil {
		d.connCache = map[string]connEntry{}
	}
	d.connCache[databaseID] = connEntry{id: db.DefaultConnectionID, expires: time.Now().Add(time.Minute)}
	d.connMu.Unlock()
	return db.DefaultConnectionID, nil
}

func NewDatasource(ctx context.Context, dsis backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	settings, err := models.LoadPluginSettings(dsis)
	if err != nil {
		return nil, err
	}

	opts, err := dsis.HTTPClientOptions(ctx)
	if err != nil {
		return nil, fmt.Errorf("http client options: %w", err)
	}
	hc, err := httpclient.New(opts)
	if err != nil {
		return nil, fmt.Errorf("http client: %w", err)
	}

	return &Datasource{
		settings:  settings,
		client:    hotdata.New(settings.ApiUrl, settings.Secrets.ApiKey, settings.WorkspaceID, hc),
		connCache: map[string]connEntry{},
	}, nil
}

func (d *Datasource) Dispose() {}

type queryModel struct {
	RawSql     string `json:"rawSql"`
	DatabaseId string `json:"databaseId"`
	Format     string `json:"format"`
	Dialect    string `json:"dialect"`
}

func (d *Datasource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	response := backend.NewQueryDataResponse()

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, q := range req.Queries {
		wg.Add(1)
		go func(q backend.DataQuery) {
			defer wg.Done()
			// A panic in a goroutine is not recovered by QueryData's caller and
			// would crash the whole plugin process, taking down every datasource
			// instance. Contain it as a failed response for this query.
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					response.Responses[q.RefID] = backend.ErrDataResponse(
						backend.StatusInternal, fmt.Sprintf("query panicked: %v", r))
					mu.Unlock()
				}
			}()
			res := d.query(ctx, q)
			mu.Lock()
			response.Responses[q.RefID] = res
			mu.Unlock()
		}(q)
	}
	wg.Wait()

	return response, nil
}

func (d *Datasource) query(ctx context.Context, query backend.DataQuery) backend.DataResponse {
	var qm queryModel
	if err := json.Unmarshal(query.JSON, &qm); err != nil {
		return backend.ErrDataResponse(backend.StatusBadRequest, fmt.Sprintf("json unmarshal: %v", err))
	}

	dbID := qm.DatabaseId
	if dbID == "" {
		dbID = d.settings.DefaultDatabaseID
	}
	if dbID == "" {
		return backend.ErrDataResponse(backend.StatusBadRequest,
			"no database selected: set one on the query or a default on the datasource")
	}

	dialect := qm.Dialect
	if dialect == "" {
		dialect = d.settings.DefaultDialect
	}

	sql := interpolateMacros(qm.RawSql, query.TimeRange, query.Interval)

	meta, err := d.client.RunQuery(ctx, dbID, sql, dialect)
	if err != nil {
		return errResponse(err)
	}

	body, err := d.client.FetchResultArrow(ctx, meta.ResultID, dbID)
	if err != nil {
		return errResponse(err)
	}
	defer func() { _ = body.Close() }()

	frame, err := FrameFromArrowStream(body)
	if err != nil {
		return backend.ErrDataResponse(backend.StatusInternal, fmt.Sprintf("decoding result: %v", err))
	}

	frame.Name = query.RefID
	frame.Meta = &data.FrameMeta{
		ExecutedQueryString: sql,
		Custom: map[string]any{
			"queryRunId":      meta.QueryRunID,
			"executionTimeMs": meta.ExecutionTimeMS,
		},
	}

	if qm.Format == "timeseries" {
		if wide, err := data.LongToWide(frame, nil); err == nil {
			frame = wide
		}
		// On conversion failure (no time column, unsorted, already wide),
		// fall through with the long frame — panels handle most shapes.
	}

	return backend.DataResponse{Frames: data.Frames{frame}}
}

func errResponse(err error) backend.DataResponse {
	var apiErr *hotdata.APIError
	if errors.As(err, &apiErr) {
		status := backend.StatusInternal
		if apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
			status = backend.StatusBadRequest
		}
		res := backend.ErrDataResponse(status, apiErr.Error())
		res.ErrorSource = backend.ErrorSourceDownstream
		return res
	}
	res := backend.ErrDataResponse(backend.StatusInternal, err.Error())
	res.ErrorSource = backend.ErrorSourceDownstream
	return res
}

// CheckHealth validates the API key and workspace, then — when a default
// database is configured — runs SELECT 1 against it. That query doubles as a
// worker warm-up: the first query against an idle workspace can take ~20s.
func (d *Datasource) CheckHealth(ctx context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	if d.settings.Secrets.ApiKey == "" {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: "API key is missing",
		}, nil
	}
	if d.settings.WorkspaceID == "" {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: "Workspace ID is missing",
		}, nil
	}

	if _, err := d.client.ListDatabases(ctx); err != nil {
		var apiErr *hotdata.APIError
		msg := fmt.Sprintf("Could not reach Hotdata: %v", err)
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
			msg = fmt.Sprintf("Authentication failed (HTTP %d) — check the API key and workspace ID.", apiErr.StatusCode)
		}
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: msg,
		}, nil
	}

	if d.settings.DefaultDatabaseID == "" {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusOk,
			Message: "Connected to Hotdata. No default database set — each query must select one.",
		}, nil
	}

	qctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := d.client.RunQuery(qctx, d.settings.DefaultDatabaseID, "SELECT 1", ""); err != nil {
		return &backend.CheckHealthResult{
			Status:  backend.HealthStatusError,
			Message: fmt.Sprintf("Connected, but test query failed: %v", err),
		}, nil
	}

	msg := "Connected to Hotdata; test query succeeded."
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		msg = fmt.Sprintf("Connected to Hotdata; test query succeeded after %.0fs (workspace worker was waking from idle).", elapsed.Seconds())
	}
	return &backend.CheckHealthResult{Status: backend.HealthStatusOk, Message: msg}, nil
}

// CallResource serves the frontend's pickers and SQL autocomplete; the API
// key never reaches the browser.
func (d *Datasource) CallResource(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	u, err := url.Parse(req.URL)
	if err != nil {
		return sendJSON(sender, http.StatusBadRequest, map[string]string{"error": "bad resource url"})
	}
	params := u.Query()

	switch req.Path {
	case "databases":
		dbs, err := d.client.ListDatabases(ctx)
		if err != nil {
			return sendJSON(sender, http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return sendJSON(sender, http.StatusOK, dbs)

	case "workspaces":
		ws, err := d.client.ListWorkspaces(ctx)
		if err != nil {
			return sendJSON(sender, http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return sendJSON(sender, http.StatusOK, ws)

	case "tables":
		tables, status, err := d.listTables(ctx, params.Get("database"), "", false)
		if err != nil {
			return sendJSON(sender, status, map[string]string{"error": err.Error()})
		}
		return sendJSON(sender, http.StatusOK, tables)

	case "schemas":
		tables, status, err := d.listTables(ctx, params.Get("database"), "", false)
		if err != nil {
			return sendJSON(sender, status, map[string]string{"error": err.Error()})
		}
		seen := map[string]bool{}
		schemas := []string{}
		for _, t := range tables {
			if !seen[t.Schema] {
				seen[t.Schema] = true
				schemas = append(schemas, t.Schema)
			}
		}
		return sendJSON(sender, http.StatusOK, schemas)

	case "columns":
		schema, table := params.Get("schema"), params.Get("table")
		if table == "" {
			return sendJSON(sender, http.StatusBadRequest, map[string]string{"error": "table is required"})
		}
		tables, status, err := d.listTables(ctx, params.Get("database"), schema, true, table)
		if err != nil {
			return sendJSON(sender, status, map[string]string{"error": err.Error()})
		}
		cols := []hotdata.Column{}
		for _, t := range tables {
			cols = append(cols, t.Columns...)
		}
		return sendJSON(sender, http.StatusOK, cols)

	default:
		return sendJSON(sender, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (d *Datasource) listTables(ctx context.Context, databaseID, schema string, includeColumns bool, table ...string) ([]hotdata.Table, int, error) {
	if databaseID == "" {
		databaseID = d.settings.DefaultDatabaseID
	}
	if databaseID == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("database is required")
	}
	connID, err := d.connectionID(ctx, databaseID)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	tbl := ""
	if len(table) > 0 {
		tbl = table[0]
	}
	tables, err := d.client.InformationSchema(ctx, connID, schema, tbl, includeColumns)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	return tables, http.StatusOK, nil
}

func sendJSON(sender backend.CallResourceResponseSender, status int, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return sender.Send(&backend.CallResourceResponse{
		Status:  status,
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    payload,
	})
}
