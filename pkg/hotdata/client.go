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
	ExecutionTimeMS int64
}

// arrowStreamType is the media type Hotdata uses for Arrow IPC streams, both
// as the Accept value on POST /v1/query and the result Content-Type.
const arrowStreamType = "application/vnd.apache.arrow.stream"

// queryRequest is the POST /v1/query body. async_after_ms is the server-side
// budget before the request degrades to a 202 + poll flow.
type queryRequest struct {
	SQL          string `json:"sql"`
	Async        bool   `json:"async"`
	AsyncAfterMS int    `json:"async_after_ms"`
	Dialect      string `json:"dialect,omitempty"`
}

// queryResponse is the JSON body of POST /v1/query: a 202 run ticket, or a
// 200 from a server that answered JSON despite the Arrow Accept header.
type queryResponse struct {
	QueryRunID      string `json:"query_run_id"`
	ResultID        string `json:"result_id"`
	ExecutionTimeMS int64  `json:"execution_time_ms"`
	Warning         string `json:"warning"`
}

type queryRunStatus struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	ResultID        string `json:"result_id"`
	ExecutionTimeMS int64  `json:"execution_time_ms"`
	ErrorMessage    string `json:"error_message"`
}

// Query executes SQL in the given database and returns the complete result
// as an Arrow IPC stream. The caller must close the reader.
//
// The request asks for Arrow directly, so a query that finishes within
// async_after_ms comes back in one round trip. Otherwise the server answers
// 202: with an X-Result-Id header when the result was too large to inline,
// or with only a run ticket when the query went async, in which case the run
// is polled until it has a result. async_after_ms has a server minimum of 1000.
func (c *Client) Query(ctx context.Context, databaseID, sql, dialect string) (RunMeta, io.ReadCloser, error) {
	body := queryRequest{SQL: sql, Async: true, AsyncAfterMS: 1000, Dialect: dialect}
	resp, err := c.doAccept(ctx, http.MethodPost, "/v1/query", databaseID, arrowStreamType, body)
	if err != nil {
		return RunMeta{}, nil, err
	}

	if resp.StatusCode == http.StatusOK && isArrow(resp.Header.Get("Content-Type")) {
		execMS, _ := strconv.ParseInt(resp.Header.Get("X-Execution-Time-Ms"), 10, 64)
		return RunMeta{
			QueryRunID:      resp.Header.Get("X-Query-Run-Id"),
			ResultID:        resp.Header.Get("X-Result-Id"),
			ExecutionTimeMS: execMS,
		}, resp.Body, nil
	}

	var qr queryResponse
	err = json.NewDecoder(resp.Body).Decode(&qr)
	_ = resp.Body.Close()
	if err != nil {
		return RunMeta{}, nil, fmt.Errorf("decoding query response: %w", err)
	}
	meta := RunMeta{QueryRunID: qr.QueryRunID, ResultID: qr.ResultID, ExecutionTimeMS: qr.ExecutionTimeMS}
	if id := resp.Header.Get("X-Result-Id"); id != "" {
		meta.ResultID = id
	}

	if resp.StatusCode == http.StatusAccepted && meta.ResultID == "" {
		if meta, err = c.pollRun(ctx, qr.QueryRunID, databaseID); err != nil {
			return RunMeta{}, nil, err
		}
	}
	if meta.ResultID == "" {
		msg := qr.Warning
		if msg == "" {
			msg = "no result id returned"
		}
		return RunMeta{}, nil, fmt.Errorf("query result unavailable: %s", msg)
	}

	rc, err := c.FetchResultArrow(ctx, meta.ResultID, databaseID)
	if err != nil {
		return RunMeta{}, nil, err
	}
	return meta, rc, nil
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
				ExecutionTimeMS: st.ExecutionTimeMS,
			}, nil
		case "failed", "cancelled":
			msg := st.ErrorMessage
			if msg == "" {
				msg = "query run " + st.Status
			}
			return RunMeta{}, fmt.Errorf("query %s: %s", st.Status, msg)
		case "queued", "pending", "running", "processing":
			// in progress — keep polling
		default:
			// A missing or unrecognized status is a malformed response; error out
			// rather than polling until the context deadline.
			return RunMeta{}, fmt.Errorf("unexpected query run status %q", st.Status)
		}
	}
}

// FetchResultArrow streams the complete result set as Arrow IPC. The endpoint
// is database-scoped. The caller must close the reader.
//
// A result that is still being persisted answers 202 with a JSON body
// {"result_id":…,"status":"processing"} and a Retry-After header, so poll
// until the response is an actual Arrow stream, backing off up to PollMax and
// honoring ctx. A failed result answers 409, which surfaces as an APIError.
func (c *Client) FetchResultArrow(ctx context.Context, resultID, databaseID string) (io.ReadCloser, error) {
	// Persistence of a just-minted result usually takes milliseconds, so start
	// with a much shorter backoff than the run-poll cadence.
	interval := 50 * time.Millisecond
	if interval > c.PollInitial {
		interval = c.PollInitial
	}
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
			Status       string `json:"status"`
			ErrorMessage string `json:"error_message"`
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
			msg := st.ErrorMessage
			if msg == "" {
				msg = strings.TrimSpace(string(raw))
			}
			return nil, fmt.Errorf("result not available (status %q): %s", st.Status, msg)
		}

		wait := retryAfterDelay(resp.Header.Get("Retry-After"), interval)
		if wait > c.PollMax {
			wait = c.PollMax
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		if interval = interval * 3 / 2; interval > c.PollMax {
			interval = c.PollMax
		}
	}
}

func isArrow(contentType string) bool {
	return strings.Contains(contentType, "arrow")
}

// maxPages bounds every paginated listing so a server that keeps answering
// has_more can never loop forever.
const maxPages = 100

// page is the cursor envelope shared by the paginated listings.
type page struct {
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

// listPages fetches path page by page, following next_cursor. decode reads
// one page's items and returns the page envelope.
func (c *Client) listPages(ctx context.Context, path string, q url.Values, decode func(io.Reader) (page, error)) error {
	for i := 0; i < maxPages; i++ {
		resp, err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), "", nil)
		if err != nil {
			return err
		}
		p, err := decode(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if !p.HasMore || p.NextCursor == "" {
			return nil
		}
		q.Set("cursor", p.NextCursor)
	}
	return nil
}

// ListDatabases returns every database in the workspace, following pages.
func (c *Client) ListDatabases(ctx context.Context) ([]Database, error) {
	var all []Database
	err := c.listPages(ctx, "/v1/databases", url.Values{"limit": {"100"}}, func(r io.Reader) (page, error) {
		var out struct {
			page
			Databases []Database `json:"databases"`
		}
		if err := json.NewDecoder(r).Decode(&out); err != nil {
			return page{}, fmt.Errorf("decoding databases: %w", err)
		}
		all = append(all, out.Databases...)
		return out.page, nil
	})
	return all, err
}

// Ping makes the cheapest workspace-scoped call, validating the API key and
// workspace id.
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/v1/databases?limit=1", "", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
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

// InformationSchema lists tables for a connection, following pages;
// schema/table narrow the listing and includeColumns adds column definitions
// with Arrow type names.
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
	var all []Table
	err := c.listPages(ctx, "/v1/information_schema", q, func(r io.Reader) (page, error) {
		var out struct {
			page
			Tables []Table `json:"tables"`
		}
		if err := json.NewDecoder(r).Decode(&out); err != nil {
			return page{}, fmt.Errorf("decoding information schema: %w", err)
		}
		all = append(all, out.Tables...)
		return out.page, nil
	})
	return all, err
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

func (c *Client) do(ctx context.Context, method, path, databaseID string, body any) (*http.Response, error) {
	return c.doAccept(ctx, method, path, databaseID, "", body)
}

// doAccept issues one request with auth headers, retrying on 429 (honoring
// Retry-After, capped at 3 attempts). Non-2xx responses become errors with a
// body snippet, since Hotdata error payloads carry stable codes worth showing.
func (c *Client) doAccept(ctx context.Context, method, path, databaseID, accept string, body any) (*http.Response, error) {
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
		if accept != "" {
			req.Header.Set("Accept", accept)
		}

		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}

		// 429 means the request was shed before execution, so any request may be
		// retried (honoring Retry-After). 502/503/504 are transient upstream
		// failures (e.g. a workspace worker mid-wake), but the response may have
		// been lost after the request executed — only idempotent (GET) requests
		// are retried on those, so a POST /v1/query never runs SQL twice.
		idempotent := method == http.MethodGet
		retryable := resp.StatusCode == http.StatusTooManyRequests ||
			(idempotent && (resp.StatusCode == http.StatusBadGateway ||
				resp.StatusCode == http.StatusServiceUnavailable ||
				resp.StatusCode == http.StatusGatewayTimeout))
		// maxRetries retries after the initial attempt => 3 requests total.
		const maxRetries = 2
		if retryable && attempt < maxRetries {
			delay := retryAfterDelay(resp.Header.Get("Retry-After"), time.Duration(attempt+1)*time.Second)
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

// retryAfterDelay parses a Retry-After header, which is either delta-seconds
// or an HTTP-date (RFC 9110 §10.2.3), falling back when absent or malformed.
func retryAfterDelay(header string, fallback time.Duration) time.Duration {
	if header == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(header); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
		return 0
	}
	return fallback
}

type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("hotdata API: HTTP %d: %s", e.StatusCode, e.message())
}

// message extracts the readable part of a Hotdata error body, which is one of
// {"error":{"code","message"}}, {"error":"code"} or, for a failed result,
// {"error_message":…}. Anything else is shown raw.
func (e *APIError) message() string {
	var b struct {
		Error        json.RawMessage `json:"error"`
		ErrorMessage string          `json:"error_message"`
	}
	if json.Unmarshal([]byte(e.Body), &b) == nil {
		var detail struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(b.Error, &detail) == nil && detail.Message != "" {
			if detail.Code != "" {
				return detail.Message + " (" + detail.Code + ")"
			}
			return detail.Message
		}
		var code string
		if json.Unmarshal(b.Error, &code) == nil && code != "" {
			return code
		}
		if b.ErrorMessage != "" {
			return b.ErrorMessage
		}
	}
	return strings.TrimSpace(e.Body)
}
