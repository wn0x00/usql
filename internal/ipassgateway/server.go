package ipassgateway

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const ExecutePath = "/v1/execute"

// Config contains the gateway dependencies. Network access control is a
// deployment responsibility; the gateway itself does not authenticate HTTP
// requests or inspect Authorization headers.
type Config struct {
	Opener DatabaseOpener
	Logger *slog.Logger
}

type server struct {
	opener DatabaseOpener
	logger *slog.Logger
}

type requestSummary struct {
	ID        string
	Operation string
	Started   time.Time
}

// NewHandler constructs the single-endpoint HTTP database gateway.
func NewHandler(config Config) (http.Handler, error) {
	if config.Opener == nil {
		return nil, errors.New("database opener is required")
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &server{opener: config.Opener, logger: logger}, nil
}

func (s *server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	summary := requestSummary{ID: newRequestID(), Started: time.Now()}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("X-Request-Id", summary.ID)

	if request.URL.Path != ExecutePath {
		s.respondError(writer, summary, &protocolError{Code: "NOT_FOUND", Message: "Endpoint not found.", Status: http.StatusNotFound})
		return
	}
	if request.URL.RawQuery != "" || request.URL.ForceQuery {
		s.respondError(writer, summary, invalidRequest("QUERY_NOT_ALLOWED", "URL query parameters are not accepted."))
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		s.respondError(writer, summary, &protocolError{Code: "METHOD_NOT_ALLOWED", Message: "Only POST is supported.", Status: http.StatusMethodNotAllowed})
		return
	}
	if !isJSONContentType(request.Header.Values("Content-Type")) {
		s.respondError(writer, summary, &protocolError{Code: "UNSUPPORTED_MEDIA_TYPE", Message: "Content-Type must be application/json.", Status: http.StatusUnsupportedMediaType})
		return
	}
	contentEncoding := strings.TrimSpace(request.Header.Get("Content-Encoding"))
	if contentEncoding != "" && !strings.EqualFold(contentEncoding, "identity") {
		s.respondError(writer, summary, &protocolError{Code: "UNSUPPORTED_CONTENT_ENCODING", Message: "Compressed request bodies are not accepted.", Status: http.StatusUnsupportedMediaType})
		return
	}
	if request.ContentLength > MaxRequestBytes {
		s.respondError(writer, summary, &protocolError{Code: "REQUEST_TOO_LARGE", Message: "Request body exceeds the size limit.", Status: http.StatusRequestEntityTooLarge})
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, MaxRequestBytes)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.respondError(writer, summary, &protocolError{Code: "REQUEST_TOO_LARGE", Message: "Request body exceeds the size limit.", Status: http.StatusRequestEntityTooLarge})
			return
		}
		s.respondError(writer, summary, invalidRequest("REQUEST_READ_FAILED", "Request body could not be read."))
		return
	}
	if len(body) == 0 {
		s.respondError(writer, summary, invalidRequest("INVALID_JSON", "Request body must be valid JSON."))
		return
	}

	envelope, protocolErr := decodeRequest(body)
	if protocolErr != nil {
		s.respondError(writer, summary, protocolErr)
		return
	}
	summary.Operation = envelope.Request.Operation
	arguments, protocolErr := envelope.Request.driverArguments()
	if protocolErr != nil {
		s.respondError(writer, summary, protocolErr)
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), envelope.Request.timeout())
	defer cancel()
	database, err := s.opener.Open(ctx, envelope.ConnectionString)
	if err != nil {
		s.respondError(writer, summary, classifyOpenError(err, ctx))
		return
	}
	defer database.Close()

	var response *responseEnvelope
	switch envelope.Request.Operation {
	case "ping":
		err = database.PingContext(ctx)
		if err == nil {
			response = &responseEnvelope{Version: protocolVersion, OK: true}
		}
	case "query":
		response, err = queryDatabase(ctx, database, *envelope.Request.SQL, arguments, envelope.Request.maxRows())
	case "exec":
		response, err = execDatabase(ctx, database, *envelope.Request.SQL, arguments)
	}
	if err != nil {
		s.respondError(writer, summary, classifyDatabaseError(envelope.Request.Operation, err, ctx))
		return
	}
	s.respond(writer, summary, http.StatusOK, response, "", len(response.Rows))
}

func (s *server) respondError(writer http.ResponseWriter, summary requestSummary, protocolErr *protocolError) {
	if protocolErr == nil {
		protocolErr = &protocolError{Code: "INTERNAL_ERROR", Message: "Database gateway request failed.", Status: http.StatusInternalServerError}
	}
	payload := &responseEnvelope{
		Version: protocolVersion,
		OK:      false,
		Error: &responseError{
			Code:      protocolErr.Code,
			Message:   protocolErr.Message,
			Retryable: protocolErr.Retryable,
		},
	}
	s.respond(writer, summary, protocolErr.Status, payload, protocolErr.Code, 0)
}

func (s *server) respond(writer http.ResponseWriter, summary requestSummary, status int, payload *responseEnvelope, errorCode string, rows int) {
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > MaxResponseBytes {
		status = http.StatusBadGateway
		errorCode = "DATABASE_RESPONSE_TOO_LARGE"
		rows = 0
		encoded, _ = json.Marshal(&responseEnvelope{
			Version: protocolVersion,
			OK:      false,
			Error: &responseError{
				Code:    errorCode,
				Message: "Database response exceeds the gateway size limit.",
			},
		})
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
	s.logger.Info(
		"database gateway request completed",
		slog.String("request_id", summary.ID),
		slog.String("operation", summary.Operation),
		slog.Int("status", status),
		slog.Int64("duration_ms", time.Since(summary.Started).Milliseconds()),
		slog.Int("rows", rows),
		slog.Int("response_bytes", len(encoded)),
		slog.String("error_code", errorCode),
	)
}

func queryDatabase(ctx context.Context, database *sql.DB, statement string, arguments []any, maxRows int) (*responseEnvelope, error) {
	rows, err := database.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columnNames, err := rows.Columns()
	if err != nil || len(columnNames) == 0 || len(columnNames) > MaxColumns {
		return nil, errors.New("invalid database columns")
	}
	columnTypes, err := rows.ColumnTypes()
	if err != nil || len(columnTypes) != len(columnNames) {
		return nil, errors.New("invalid database column metadata")
	}
	columns := make([]responseColumn, len(columnTypes))
	for index, columnType := range columnTypes {
		columns[index] = encodeColumn(columnNames[index], columnType)
	}
	response := &responseEnvelope{
		Version:      protocolVersion,
		OK:           true,
		Columns:      columns,
		AffectedRows: "0",
	}
	metadata, err := json.Marshal(columns)
	if err != nil || len(metadata) > MaxResponseBytes-responseBudgetMargin {
		return nil, errors.New("database metadata exceeds response limit")
	}
	usedBytes := len(metadata) + responseBudgetMargin

	for rows.Next() {
		if len(response.Rows) >= maxRows {
			response.Truncated = true
			response.Warnings = append(response.Warnings, "ROW_LIMIT_REACHED")
			break
		}
		rawValues := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for index := range rawValues {
			destinations[index] = &rawValues[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, err
		}
		encodedRow := make([]responseCell, len(rawValues))
		for index, value := range rawValues {
			cell, err := encodeCell(value, columns[index].DatabaseType)
			if err != nil {
				return nil, err
			}
			encodedRow[index] = cell
		}
		rowJSON, err := json.Marshal(encodedRow)
		if err != nil {
			return nil, errors.New("database row could not be encoded")
		}
		if usedBytes+len(rowJSON)+1 > MaxResponseBytes {
			response.Truncated = true
			response.Warnings = append(response.Warnings, "RESPONSE_SIZE_LIMIT_REACHED")
			break
		}
		usedBytes += len(rowJSON) + 1
		response.Rows = append(response.Rows, encodedRow)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return response, nil
}

func execDatabase(ctx context.Context, database *sql.DB, statement string, arguments []any) (*responseEnvelope, error) {
	result, err := database.ExecContext(ctx, statement, arguments...)
	if err != nil {
		return nil, err
	}
	response := &responseEnvelope{Version: protocolVersion, OK: true, AffectedRows: "0"}
	if affectedRows, err := result.RowsAffected(); err == nil {
		if affectedRows < 0 {
			return nil, errors.New("database returned an invalid affected row count")
		}
		response.AffectedRows = strconv.FormatInt(affectedRows, 10)
	}
	if lastInsertID, err := result.LastInsertId(); err == nil {
		encoded := strconv.FormatInt(lastInsertID, 10)
		response.LastInsertID = &encoded
	}
	return response, nil
}

func encodeColumn(name string, columnType *sql.ColumnType) responseColumn {
	databaseType := strings.ToUpper(strings.TrimSpace(columnType.DatabaseTypeName()))
	column := responseColumn{Name: name, DatabaseType: databaseType}
	if nullable, ok := columnType.Nullable(); ok {
		column.Nullable = &nullable
	}
	if length, ok := columnType.Length(); ok {
		column.Length = &length
	}
	if precision, scale, ok := columnType.DecimalSize(); ok {
		column.Precision = &precision
		column.Scale = &scale
	}
	return column
}

func encodeCell(value any, databaseType string) (responseCell, error) {
	switch typed := value.(type) {
	case nil:
		return responseCell{Type: "null", Value: nil}, nil
	case bool:
		return responseCell{Type: "bool", Value: typed}, nil
	case int64:
		return responseCell{Type: "int64", Value: strconv.FormatInt(typed, 10)}, nil
	case int32:
		return responseCell{Type: "int64", Value: strconv.FormatInt(int64(typed), 10)}, nil
	case int:
		return responseCell{Type: "int64", Value: strconv.FormatInt(int64(typed), 10)}, nil
	case uint64:
		if typed > math.MaxInt64 {
			return responseCell{}, errors.New("unsigned integer exceeds protocol range")
		}
		return responseCell{Type: "int64", Value: strconv.FormatUint(typed, 10)}, nil
	case uint32:
		return responseCell{Type: "int64", Value: strconv.FormatUint(uint64(typed), 10)}, nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return responseCell{}, errors.New("database returned a non-finite number")
		}
		return responseCell{Type: "float64", Value: strconv.FormatFloat(typed, 'g', -1, 64)}, nil
	case float32:
		value64 := float64(typed)
		if math.IsNaN(value64) || math.IsInf(value64, 0) {
			return responseCell{}, errors.New("database returned a non-finite number")
		}
		return responseCell{Type: "float64", Value: strconv.FormatFloat(value64, 'g', -1, 32)}, nil
	case string:
		if len(typed) > MaxResponseBytes-responseBudgetMargin {
			return responseCell{}, errors.New("database cell exceeds response limit")
		}
		return responseCell{Type: "string", Value: typed}, nil
	case []byte:
		if len(typed) > MaxResponseBytes-responseBudgetMargin {
			return responseCell{}, errors.New("database cell exceeds response limit")
		}
		if isTextDatabaseType(databaseType) && utf8.Valid(typed) {
			return responseCell{Type: "string", Value: string(typed)}, nil
		}
		return responseCell{Type: "bytes", Value: base64.StdEncoding.EncodeToString(typed)}, nil
	case time.Time:
		return responseCell{Type: "time", Value: typed.Format(time.RFC3339Nano)}, nil
	default:
		return responseCell{}, errors.New("database returned an unsupported value type")
	}
}

func isTextDatabaseType(databaseType string) bool {
	upper := strings.ToUpper(databaseType)
	for _, marker := range []string{"CHAR", "TEXT", "CLOB", "JSON", "XML", "DECIMAL", "NUMERIC", "DATE", "TIME"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func classifyOpenError(err error, ctx context.Context) *protocolError {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &protocolError{Code: "DATABASE_TIMEOUT", Message: "Database operation timed out.", Status: http.StatusGatewayTimeout, Retryable: true}
	}
	switch {
	case errors.Is(err, ErrConnectionStringInvalid):
		return invalidRequest("INVALID_CONNECTION", "Database connection string is invalid.")
	case errors.Is(err, ErrDriverNotAllowed):
		return &protocolError{Code: "DRIVER_NOT_ALLOWED", Message: "Database driver is not allowed.", Status: http.StatusForbidden}
	case errors.Is(err, ErrDriverUnavailable):
		return &protocolError{Code: "DRIVER_UNAVAILABLE", Message: "Database driver is unavailable in this gateway build.", Status: http.StatusServiceUnavailable, Retryable: true}
	default:
		return &protocolError{Code: "DATABASE_CONNECTION_FAILED", Message: "Database connection could not be opened.", Status: http.StatusBadGateway, Retryable: true}
	}
}

func classifyDatabaseError(operation string, err error, ctx context.Context) *protocolError {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &protocolError{Code: "DATABASE_TIMEOUT", Message: "Database operation timed out.", Status: http.StatusGatewayTimeout, Retryable: true}
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return &protocolError{Code: "REQUEST_CANCELED", Message: "Database operation was canceled.", Status: http.StatusRequestTimeout, Retryable: true}
	}
	if operation == "ping" {
		return &protocolError{Code: "DATABASE_CONNECTION_FAILED", Message: "Database connection check failed.", Status: http.StatusBadGateway, Retryable: true}
	}
	return &protocolError{Code: "DATABASE_OPERATION_FAILED", Message: "Database operation failed.", Status: http.StatusUnprocessableEntity}
}

func isJSONContentType(values []string) bool {
	if len(values) != 1 {
		return false
	}
	mediaType, parameters, err := mime.ParseMediaType(values[0])
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return false
	}
	for name, value := range parameters {
		if !strings.EqualFold(name, "charset") || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

// IsLoopbackListenAddress reports whether addr is an explicit TCP loopback
// listener. Empty/wildcard hosts and arbitrary DNS names are never trusted.
func IsLoopbackListenAddress(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return false
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	parsed, err := netip.ParseAddr(host)
	return err == nil && parsed.IsLoopback()
}

func newRequestID() string {
	var raw [16]byte
	for index := range raw {
		raw[index] = byte(rand.UintN(256))
	}
	return hex.EncodeToString(raw[:])
}
