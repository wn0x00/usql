// Package transport implements the database/sql side of the Yingdao iPaaS
// managed SQL transport. It never accepts or resolves a database credential.
package transport

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// BaseURLEnv is injected by the sandbox runner. In production its path
	// contains a short-lived run capability bound to exactly one authorization.
	BaseURLEnv = "USQL_IPASS_BASE_URL"

	protocolVersion  = 1
	requestTimeout   = 120 * time.Second
	maxRequestBytes  = 1 * 1024 * 1024
	maxSQLBytes      = 256 * 1024
	maxParameters    = 1024
	maxResponseBytes = 16 * 1024 * 1024
	maxColumns       = 512
	maxRows          = 1000
)

var (
	aliasPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	capabilityPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)
	pathSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
)

// Driver implements database/sql/driver.Driver and DriverContext.
type Driver struct{}

var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.DriverContext = (*Driver)(nil)
)

// Open implements driver.Driver.
func (d *Driver) Open(name string) (driver.Conn, error) {
	connector, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return connector.Connect(context.Background())
}

// OpenConnector validates the non-secret alias and the injected Adapter URL.
func (d *Driver) OpenConnector(name string) (driver.Connector, error) {
	alias := strings.TrimSpace(name)
	if !aliasPattern.MatchString(alias) {
		return nil, errors.New("invalid iPaaS connection alias")
	}
	baseURL, err := adapterBaseURL(os.Getenv(BaseURLEnv))
	if err != nil {
		return nil, err
	}
	return &connector{
		driver:  d,
		alias:   alias,
		baseURL: baseURL,
		client:  newHTTPClient(),
	}, nil
}

type connector struct {
	driver  *Driver
	alias   string
	baseURL string
	client  *http.Client
}

var _ driver.Connector = (*connector)(nil)

func (c *connector) Connect(context.Context) (driver.Conn, error) {
	return &conn{connector: c}, nil
}

func (c *connector) Driver() driver.Driver {
	return c.driver
}

type conn struct {
	connector *connector
	closed    atomic.Bool
}

var (
	_ driver.Conn            = (*conn)(nil)
	_ driver.ConnBeginTx     = (*conn)(nil)
	_ driver.ExecerContext   = (*conn)(nil)
	_ driver.Pinger          = (*conn)(nil)
	_ driver.QueryerContext  = (*conn)(nil)
	_ driver.SessionResetter = (*conn)(nil)
	_ driver.Validator       = (*conn)(nil)
)

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	return &stmt{conn: c, query: query}, nil
}

func (c *conn) Close() error {
	c.closed.Store(true)
	return nil
}

func (c *conn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported by the stateless iPaaS transport")
}

func (c *conn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return nil, errors.New("transactions are not supported by the stateless iPaaS transport")
}

func (c *conn) Ping(ctx context.Context) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	_, err := c.connector.call(ctx, requestEnvelope{
		Version:   protocolVersion,
		Operation: "ping",
		Alias:     c.connector.alias,
	})
	return err
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	if err := validateStatement(query, args); err != nil {
		return nil, err
	}
	params, err := encodeParameters(args)
	if err != nil {
		return nil, err
	}
	response, err := c.connector.call(ctx, requestEnvelope{
		Version:    protocolVersion,
		Operation:  "query",
		Alias:      c.connector.alias,
		SQL:        query,
		Parameters: params,
		Options: &requestOptions{
			MaxRows:   maxRows,
			TimeoutMS: int(requestTimeout / time.Millisecond),
		},
	})
	if err != nil {
		return nil, err
	}
	return newRows(response)
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	if err := validateStatement(query, args); err != nil {
		return nil, err
	}
	params, err := encodeParameters(args)
	if err != nil {
		return nil, err
	}
	response, err := c.connector.call(ctx, requestEnvelope{
		Version:    protocolVersion,
		Operation:  "exec",
		Alias:      c.connector.alias,
		SQL:        query,
		Parameters: params,
		Options: &requestOptions{
			TimeoutMS: int(requestTimeout / time.Millisecond),
		},
	})
	if err != nil {
		return nil, err
	}
	return newResult(response)
}

func (c *conn) ResetSession(context.Context) error {
	return c.checkOpen()
}

func (c *conn) IsValid() bool {
	return !c.closed.Load()
}

func (c *conn) checkOpen() error {
	if c.closed.Load() {
		return driver.ErrBadConn
	}
	return nil
}

type stmt struct {
	conn  *conn
	query string
}

var (
	_ driver.Stmt             = (*stmt)(nil)
	_ driver.StmtExecContext  = (*stmt)(nil)
	_ driver.StmtQueryContext = (*stmt)(nil)
)

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), valuesToNamed(args))
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), valuesToNamed(args))
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
}

func valuesToNamed(values []driver.Value) []driver.NamedValue {
	result := make([]driver.NamedValue, len(values))
	for i, value := range values {
		result[i] = driver.NamedValue{Ordinal: i + 1, Value: value}
	}
	return result
}

type requestEnvelope struct {
	Version    int                `json:"version"`
	Operation  string             `json:"operation"`
	Alias      string             `json:"alias"`
	SQL        string             `json:"sql,omitempty"`
	Parameters []requestParameter `json:"parameters,omitempty"`
	Options    *requestOptions    `json:"options,omitempty"`
}

type requestOptions struct {
	MaxRows   int `json:"maxRows,omitempty"`
	TimeoutMS int `json:"timeoutMs"`
}

type requestParameter struct {
	Name    string      `json:"name,omitempty"`
	Ordinal int         `json:"ordinal"`
	Type    string      `json:"type"`
	Value   interface{} `json:"value"`
}

type responseEnvelope struct {
	Version      int              `json:"version"`
	OK           bool             `json:"ok"`
	Columns      []responseColumn `json:"columns"`
	Rows         [][]responseCell `json:"rows"`
	AffectedRows string           `json:"affectedRows"`
	LastInsertID *string          `json:"lastInsertId"`
	Truncated    bool             `json:"truncated"`
	Warnings     []string         `json:"warnings"`
	Error        *wireError       `json:"error"`
}

type responseColumn struct {
	Name         string `json:"name"`
	DatabaseType string `json:"databaseType"`
	Nullable     *bool  `json:"nullable"`
	Length       *int64 `json:"length"`
	Precision    *int64 `json:"precision"`
	Scale        *int64 `json:"scale"`
}

type responseCell struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

type wireError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// RemoteError is a sanitized error returned by the Adapter or database
// gateway. It intentionally contains neither an authorization ID nor a DSN.
type RemoteError struct {
	Code      string
	Message   string
	Retryable bool
	Status    int
}

func (e *RemoteError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

func (c *connector) call(ctx context.Context, payload requestEnvelope) (*responseEnvelope, error) {
	requestBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode iPaaS request: %w", err)
	}
	if len(requestBody) > maxRequestBytes {
		return nil, errors.New("iPaaS request exceeds the size limit")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/execute", bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("create iPaaS request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "usql-ipass/1")

	response, err := c.client.Do(request)
	if err != nil {
		// net/http wraps transport errors in url.Error, whose Error method
		// includes the full request URL. The BASE_URL path contains the
		// short-lived run capability, so never propagate that wrapper.
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("iPaaS Adapter request timed out")
		}
		return nil, errors.New("iPaaS Adapter request failed")
	}
	defer response.Body.Close()

	body, err := readBounded(response.Body, maxResponseBytes)
	if err != nil {
		return nil, err
	}
	var envelope responseEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, errors.New("iPaaS Adapter returned an invalid JSON response")
	}
	if envelope.Version != protocolVersion {
		return nil, errors.New("iPaaS Adapter returned an unsupported protocol version")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.OK {
		remote := &RemoteError{Status: response.StatusCode}
		if envelope.Error != nil {
			remote.Code = cleanErrorText(envelope.Error.Code, 128)
			remote.Message = cleanErrorText(strings.ReplaceAll(envelope.Error.Message, c.baseURL, "[redacted Adapter URL]"), 2048)
			remote.Retryable = envelope.Error.Retryable
		}
		if remote.Code == "" {
			remote.Code = "IPASS_HTTP_ERROR"
		}
		if remote.Message == "" {
			remote.Message = fmt.Sprintf("iPaaS Adapter returned HTTP %d", response.StatusCode)
		}
		return nil, remote
	}
	if len(envelope.Columns) > maxColumns || len(envelope.Rows) > maxRows {
		return nil, errors.New("iPaaS Adapter response exceeds the configured row or column limit")
	}
	if envelope.Truncated {
		return nil, errors.New("iPaaS Adapter truncated the SQL result; narrow the query")
	}
	return &envelope, nil
}

func adapterBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%s is required", BaseURLEnv)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Path == "" {
		return "", fmt.Errorf("%s is invalid", BaseURLEnv)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s must not contain user info, query, or fragment", BaseURLEnv)
	}
	escapedPath := parsed.EscapedPath()
	if strings.Contains(escapedPath, "%") {
		return "", fmt.Errorf("%s path must not contain percent-encoded segments", BaseURLEnv)
	}
	segments := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(segments) < 4 || segments[len(segments)-4] != "v1" || segments[len(segments)-3] != "runs" ||
		!capabilityPattern.MatchString(segments[len(segments)-2]) || segments[len(segments)-1] != "db" {
		return "", fmt.Errorf("%s must end with /v1/runs/<capability>/db", BaseURLEnv)
	}
	for _, segment := range segments {
		if segment == "." || segment == ".." || !pathSegmentPattern.MatchString(segment) {
			return "", fmt.Errorf("%s contains an invalid path segment", BaseURLEnv)
		}
	}
	host := strings.ToLower(parsed.Hostname())
	switch parsed.Scheme {
	case "https":
	case "http":
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", fmt.Errorf("%s only permits cleartext HTTP on loopback", BaseURLEnv)
		}
	default:
		return "", fmt.Errorf("%s must use HTTPS or loopback HTTP", BaseURLEnv)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func newHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          8,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: requestTimeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, errors.New("failed to read iPaaS Adapter response")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("iPaaS Adapter response exceeds the size limit")
	}
	return data, nil
}

func cleanErrorText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

func encodeParameters(values []driver.NamedValue) ([]requestParameter, error) {
	result := make([]requestParameter, len(values))
	for i, named := range values {
		param := requestParameter{Name: named.Name, Ordinal: named.Ordinal}
		if param.Ordinal <= 0 {
			param.Ordinal = i + 1
		}
		switch value := named.Value.(type) {
		case nil:
			param.Type, param.Value = "null", nil
		case bool:
			param.Type, param.Value = "bool", value
		case int64:
			param.Type, param.Value = "int64", strconv.FormatInt(value, 10)
		case float64:
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, errors.New("NaN and infinity are not supported SQL parameters")
			}
			param.Type, param.Value = "float64", strconv.FormatFloat(value, 'g', -1, 64)
		case string:
			param.Type, param.Value = "string", value
		case []byte:
			param.Type, param.Value = "bytes", base64.StdEncoding.EncodeToString(value)
		case time.Time:
			param.Type, param.Value = "time", value.Format(time.RFC3339Nano)
		default:
			return nil, fmt.Errorf("unsupported SQL parameter type %T", value)
		}
		result[i] = param
	}
	return result, nil
}

func validateStatement(query string, values []driver.NamedValue) error {
	if len(query) > maxSQLBytes {
		return errors.New("SQL statement exceeds the size limit")
	}
	if len(values) > maxParameters {
		return errors.New("SQL parameter count exceeds the limit")
	}
	return nil
}

type rows struct {
	columns []responseColumn
	values  [][]driver.Value
	index   int
	closed  bool
}

var (
	_ driver.Rows                           = (*rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
	_ driver.RowsColumnTypeLength           = (*rows)(nil)
	_ driver.RowsColumnTypeNullable         = (*rows)(nil)
	_ driver.RowsColumnTypePrecisionScale   = (*rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*rows)(nil)
)

func newRows(response *responseEnvelope) (*rows, error) {
	if len(response.Columns) == 0 && len(response.Rows) != 0 {
		return nil, errors.New("iPaaS Adapter returned rows without columns")
	}
	result := &rows{columns: response.Columns, values: make([][]driver.Value, len(response.Rows))}
	for rowIndex, sourceRow := range response.Rows {
		if len(sourceRow) != len(response.Columns) {
			return nil, errors.New("iPaaS Adapter returned a row with an invalid column count")
		}
		result.values[rowIndex] = make([]driver.Value, len(sourceRow))
		for columnIndex, cell := range sourceRow {
			value, err := decodeCell(cell)
			if err != nil {
				return nil, fmt.Errorf("decode row %d column %d: %w", rowIndex+1, columnIndex+1, err)
			}
			result.values[rowIndex][columnIndex] = value
		}
	}
	return result, nil
}

func (r *rows) Columns() []string {
	result := make([]string, len(r.columns))
	for i, column := range r.columns {
		result[i] = column.Name
	}
	return result
}

func (r *rows) Close() error {
	r.closed = true
	r.values = nil
	return nil
}

func (r *rows) Next(destination []driver.Value) error {
	if r.closed || r.index >= len(r.values) {
		return io.EOF
	}
	if len(destination) < len(r.columns) {
		return errors.New("SQL destination has fewer values than columns")
	}
	copy(destination, r.values[r.index])
	r.index++
	return nil
}

func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	if index < 0 || index >= len(r.columns) {
		return ""
	}
	return r.columns[index].DatabaseType
}

func (r *rows) ColumnTypeLength(index int) (int64, bool) {
	if index < 0 || index >= len(r.columns) || r.columns[index].Length == nil {
		return 0, false
	}
	return *r.columns[index].Length, true
}

func (r *rows) ColumnTypeNullable(index int) (bool, bool) {
	if index < 0 || index >= len(r.columns) || r.columns[index].Nullable == nil {
		return false, false
	}
	return *r.columns[index].Nullable, true
}

func (r *rows) ColumnTypePrecisionScale(index int) (int64, int64, bool) {
	if index < 0 || index >= len(r.columns) || r.columns[index].Precision == nil || r.columns[index].Scale == nil {
		return 0, 0, false
	}
	return *r.columns[index].Precision, *r.columns[index].Scale, true
}

func (r *rows) ColumnTypeScanType(index int) reflect.Type {
	if index < 0 || index >= len(r.columns) {
		return reflect.TypeOf("")
	}
	switch strings.ToUpper(r.columns[index].DatabaseType) {
	case "BOOL", "BOOLEAN":
		return reflect.TypeOf(false)
	case "BIGINT", "INT8", "INTEGER", "INT", "SMALLINT", "TINYINT":
		return reflect.TypeOf(int64(0))
	case "DOUBLE", "FLOAT", "FLOAT4", "FLOAT8", "REAL":
		return reflect.TypeOf(float64(0))
	case "BINARY", "BLOB", "BYTEA", "VARBINARY":
		return reflect.TypeOf([]byte(nil))
	case "DATE", "DATETIME", "TIMESTAMP", "TIMESTAMPTZ":
		return reflect.TypeOf(time.Time{})
	default:
		return reflect.TypeOf("")
	}
}

func decodeCell(cell responseCell) (driver.Value, error) {
	decodeString := func() (string, error) {
		var value string
		if err := json.Unmarshal(cell.Value, &value); err != nil {
			return "", errors.New("typed SQL value must be a string")
		}
		return value, nil
	}
	switch strings.ToLower(cell.Type) {
	case "null":
		return nil, nil
	case "bool":
		var value bool
		if err := json.Unmarshal(cell.Value, &value); err != nil {
			return nil, errors.New("invalid bool SQL value")
		}
		return value, nil
	case "int64":
		value, err := decodeString()
		if err != nil {
			return nil, err
		}
		return strconv.ParseInt(value, 10, 64)
	case "float64":
		value, err := decodeString()
		if err != nil {
			return nil, err
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return nil, errors.New("invalid float64 SQL value")
		}
		return parsed, nil
	case "bytes":
		value, err := decodeString()
		if err != nil {
			return nil, err
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(value)
		if err != nil {
			return nil, errors.New("invalid base64 SQL value")
		}
		return decoded, nil
	case "time":
		value, err := decodeString()
		if err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, errors.New("invalid RFC3339 SQL time value")
		}
		return parsed, nil
	case "string", "text", "json", "decimal", "date", "datetime":
		return decodeString()
	default:
		return nil, fmt.Errorf("unsupported typed SQL value %q", cell.Type)
	}
}

type result struct {
	affectedRows int64
	lastInsertID *int64
}

var _ driver.Result = (*result)(nil)

func newResult(response *responseEnvelope) (*result, error) {
	var affectedRows int64
	var err error
	if response.AffectedRows != "" {
		affectedRows, err = strconv.ParseInt(response.AffectedRows, 10, 64)
		if err != nil || affectedRows < 0 {
			return nil, errors.New("iPaaS Adapter returned an invalid affected row count")
		}
	}
	result := &result{affectedRows: affectedRows}
	if response.LastInsertID != nil {
		value, err := strconv.ParseInt(*response.LastInsertID, 10, 64)
		if err != nil {
			return nil, errors.New("iPaaS Adapter returned an invalid last insert ID")
		}
		result.lastInsertID = &value
	}
	return result, nil
}

func (r *result) LastInsertId() (int64, error) {
	if r.lastInsertID == nil {
		return 0, errors.New("last insert ID is unavailable")
	}
	return *r.lastInsertID, nil
}

func (r *result) RowsAffected() (int64, error) {
	return r.affectedRows, nil
}
