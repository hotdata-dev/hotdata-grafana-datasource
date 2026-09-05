// Package hotdata is a client for the Hotdata v1 HTTP API, covering the
// endpoints the datasource needs: query execution (sync and async), Arrow
// result retrieval, and catalog discovery.
package hotdata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL     string // without trailing slash or /v1
	apiKey      string
	workspaceID string
	hc          *http.Client

	// Polling cadence for async query runs; overridable in tests.
	PollInitial time.Duration
	PollMax     time.Duration
}

func New(baseURL, apiKey, workspaceID string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{
		baseURL:     baseURL,
		apiKey:      apiKey,
		workspaceID: workspaceID,
		hc:          hc,
		PollInitial: 500 * time.Millisecond,
		PollMax:     2 * time.Second,
	}
}

type Database struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	DefaultCatalog string `json:"default_catalog"`
	DefaultSchema  string `json:"default_schema"`
}

type DatabaseDetail struct {
	Database
	DefaultConnectionID string `json:"default_connection_id"`
}

type Column struct {
	Name     string `json:"name"`
	DataType string `json:"data_type"` // Arrow type name, e.g. Utf8View, Int64
	Nullable bool   `json:"nullable"`
}

type Table struct {
	Schema  string   `json:"schema"`
	Table   string   `json:"table"`
	Synced  bool     `json:"synced"`
	Columns []Column `json:"columns,omitempty"`
}

type Workspace struct {
	PublicID string `json:"public_id"`
	Name     string `json:"name"`
}

type RunMeta struct {
	QueryRunID      string
	ResultID        string
	RowCount        int64
	ExecutionTimeMS int64
}

// queryRequest is the POST /v1/query body. async_after_ms is the server-side
// budget before the request degrades to a 202 + poll flow.
type queryRequest struct {
	SQL          string `json:"sql"`
	Async        bool   `json:"async"`
	AsyncAfterMS int    `json:"async_after_ms"`
	Dialect      string `json:"dialect,omitempty"`
}

type queryResponse struct {
	QueryRunID      string `json:"query_run_id"`
	ResultID        string `json:"result_id"`
	RowCount        int64  `json:"row_count"`
	ExecutionTimeMS int64  `json:"execution_time_ms"`
	// 202 fields
	Status    string `json:"status"`
	StatusURL string `json:"status_url"`
}

type queryRunStatus struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	ResultID        string `json:"result_id"`
	RowCount        int64  `json:"row_count"`
	ExecutionTimeMS int64  `json:"execution_time_ms"`
	Error           string `json:"error"`
	Message         string `json:"message"`
}

// RunQuery executes SQL in the given database and blocks until the run is
// terminal, following the async 202 + poll flow when the server chooses it.
// async_after_ms has a server-enforced minimum of 1000.
func (c *Client) RunQuery(ctx context.Context, databaseID, sql, dialect string) (RunMeta, error) {
	body := queryRequest{SQL: sql, Async: true, AsyncAfterMS: 1000, Dialect: dialect}
	resp, err := c.do(ctx, http.MethodPost, "/v1/query", databaseID, body)
	if err != nil {
		return RunMeta{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	var qr queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		return RunMeta{}, fmt.Errorf("decoding query response: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		return RunMeta{
			QueryRunID:      qr.QueryRunID,
			ResultID:        qr.ResultID,
			RowCount:        qr.RowCount,
			ExecutionTimeMS: qr.ExecutionTimeMS,
		}, nil
	}

	// 202: poll /v1/query-runs/{id} until terminal.
	return c.pollRun(ctx, qr.QueryRunID, databaseID)
}

// pollRun polls a query run until terminal. The endpoint is database-scoped,
// so the x-database-id header is required here too.
func (c *Client) pollRun(ctx context.Context, runID, databaseID string) (RunMeta, error) {
	interval := c.PollInitial
	for {
		select {
		case <-ctx.Done():
			return RunMeta{}, ctx.Err()
		case <-time.After(interval):
		}
		if interval = interval * 3 / 2; interval > c.PollMax {
			interval = c.PollMax
		}

		resp, err := c.do(ctx, http.MethodGet, "/v1/query-runs/"+runID, databaseID, nil)
		if err != nil {
			return RunMeta{}, err
		}
		var st queryRunStatus
		err = json.NewDecoder(resp.Body).Decode(&st)
		_ = resp.Body.Close()
		if err != nil {
			return RunMeta{}, fmt.Errorf("decoding query run status: %w", err)
		}

		switch st.Status {
		case "succeeded":
			return RunMeta{
				QueryRunID:      st.ID,
				ResultID:        st.ResultID,
				RowCount:        st.RowCount,
				ExecutionTimeMS: st.ExecutionTimeMS,
			}, nil
		case "failed", "cancelled":
			msg := st.Error
			if msg == "" {
				msg = st.Message
			}
			if msg == "" {
				msg = "query run " + st.Status
			}
			return RunMeta{}, fmt.Errorf("query %s: %s", st.Status, msg)
		}
	}
}

// FetchResultArrow streams the complete result set as Arrow IPC. The endpoint
// is database-scoped. The caller must close the reader.
//
// A freshly created result is not immediately materialized: the endpoint
// answers HTTP 200 with a JSON body {"result_id":…,"status":"processing"}
// rather than blocking. This happens on the fast/sync query path, where the
// fetch can land within milliseconds of the result id being minted. So poll
// until the response is an actual Arrow stream, backing off up to PollMax and
// honoring ctx.
func (c *Client) FetchResultArrow(ctx context.Context, resultID, databaseID string) (io.ReadCloser, error) {
	interval := c.PollInitial
	for {
		resp, err := c.do(ctx, http.MethodGet, "/v1/results/"+resultID+"?format=arrow", databaseID, nil)
		if err != nil {
			return nil, err
		}

		if isArrow(resp.Header.Get("Content-Type")) {
			return resp.Body, nil
		}

		// Not arrow yet — a JSON status placeholder. Read the body so an
		// unrecognized shape becomes an error rather than an endless poll.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
		var st struct {
			Status  string `json:"status"`
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, fmt.Errorf("unexpected non-arrow result response: %s", strings.TrimSpace(string(raw)))
		}
		// Only known in-progress states are polled; anything else (including a
		// missing status) is terminal, so we never spin on an unexpected body.
		switch st.Status {
		case "processing", "pending", "running":
			// keep polling
		default:
			msg := st.Error
			if msg == "" {
				msg = st.Message
			}
			if msg == "" {
				msg = strings.TrimSpace(string(raw))
			}
			return nil, fmt.Errorf("result not available (status %q): %s", st.Status, msg)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if interval = interval * 3 / 2; interval > c.PollMax {
			interval = c.PollMax
		}
	}
}

func isArrow(contentType string) bool {
	return strings.Contains(contentType, "arrow")
}

func (c *Client) ListDatabases(ctx context.Context) ([]Database, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/databases", "", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Databases []Database `json:"databases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding databases: %w", err)
	}
	return out.Databases, nil
}

// GetDatabase resolves one database, including the default_connection_id that
// the information_schema endpoint keys on.
func (c *Client) GetDatabase(ctx context.Context, id string) (DatabaseDetail, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/databases/"+url.PathEscape(id), "", nil)
	if err != nil {
		return DatabaseDetail{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out DatabaseDetail
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return DatabaseDetail{}, fmt.Errorf("decoding database: %w", err)
	}
	return out, nil
}

// InformationSchema lists tables for a connection; schema/table narrow the
// listing and includeColumns adds column definitions with Arrow type names.
func (c *Client) InformationSchema(ctx context.Context, connectionID, schema, table string, includeColumns bool) ([]Table, error) {
	q := url.Values{"connection_id": {connectionID}}
	if schema != "" {
		q.Set("schema", schema)
	}
	if table != "" {
		q.Set("table", table)
	}
	if includeColumns {
		q.Set("include_columns", "true")
	}
	resp, err := c.do(ctx, http.MethodGet, "/v1/information_schema?"+q.Encode(), "", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Tables []Table `json:"tables"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding information schema: %w", err)
	}
	return out.Tables, nil
}

func (c *Client) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/workspaces", "", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding workspaces: %w", err)
	}
	return out.Workspaces, nil
}

// do issues one request with auth headers, retrying on 429 (honoring
// Retry-After, capped at 3 attempts). Non-2xx responses become errors with a
// body snippet, since Hotdata error payloads carry stable codes worth showing.
func (c *Client) do(ctx context.Context, method, path, databaseID string, body any) (*http.Response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}

	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("x-workspace-id", c.workspaceID)
		if databaseID != "" {
			req.Header.Set("x-database-id", databaseID)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}

		// 429: shed load, honor Retry-After. 502/503/504: transient upstream
		// failures (e.g. a workspace worker mid-wake) — brief backoff. Queries
		// are read-only, so re-POSTing /v1/query is safe.
		retryable := resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusBadGateway ||
			resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout
		// maxRetries retries after the initial attempt => 3 requests total.
		const maxRetries = 2
		if retryable && attempt < maxRetries {
			delay := time.Duration(attempt+1) * time.Second
			if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra >= 0 {
				delay = time.Duration(ra) * time.Second
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			continue
		}

		if resp.StatusCode >= 400 {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, &APIError{StatusCode: resp.StatusCode, Body: string(snippet)}
		}
		return resp, nil
	}
}

type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("hotdata API: HTTP %d: %s", e.StatusCode, e.Body)
}
