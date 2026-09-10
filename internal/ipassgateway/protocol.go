package ipassgateway

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	protocolVersion       = 1
	MaxRequestBytes       = int64(1024 * 1024)
	MaxResponseBytes      = 8 * 1024 * 1024
	MaxConnectionBytes    = 16 * 1024
	MaxSQLBytes           = 256 * 1024
	MaxParameterCount     = 1024
	MaxParameterValueSize = 256 * 1024
	MaxRows               = 10_000
	MaxColumns            = 1024
	MaxTimeout            = 180 * time.Second
	defaultTimeout        = 120 * time.Second
	defaultMaxRows        = 1000
	maxJSONDepth          = 64
	responseBudgetMargin  = 64 * 1024
)

var (
	aliasPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	parameterNamePattern = regexp.MustCompile(`^(?:[A-Za-z_][A-Za-z0-9_]{0,127})?$`)
	int64Pattern         = regexp.MustCompile(`^-?(?:0|[1-9][0-9]{0,18})$`)
	float64Pattern       = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)
)

type outerRequest struct {
	Version          int             `json:"version"`
	ConnectionString string          `json:"connectionString"`
	Request          requestEnvelope `json:"request"`
}

type requestEnvelope struct {
	Version    int                 `json:"version"`
	Operation  string              `json:"operation"`
	Alias      string              `json:"alias"`
	SQL        *string             `json:"sql,omitempty"`
	Parameters *[]requestParameter `json:"parameters,omitempty"`
	Options    *requestOptions     `json:"options,omitempty"`
}

type requestOptions struct {
	MaxRows   *int `json:"maxRows,omitempty"`
	TimeoutMS *int `json:"timeoutMs,omitempty"`
}

type requestParameter struct {
	Name    string          `json:"name"`
	Ordinal int             `json:"ordinal"`
	Type    string          `json:"type"`
	Value   json.RawMessage `json:"value"`
}

type responseEnvelope struct {
	Version      int              `json:"version"`
	OK           bool             `json:"ok"`
	Columns      []responseColumn `json:"columns,omitempty"`
	Rows         [][]responseCell `json:"rows,omitempty"`
	AffectedRows string           `json:"affectedRows,omitempty"`
	LastInsertID *string          `json:"lastInsertId,omitempty"`
	Truncated    bool             `json:"truncated,omitempty"`
	Warnings     []string         `json:"warnings,omitempty"`
	Error        *responseError   `json:"error,omitempty"`
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
	Type  string `json:"type"`
	Value any    `json:"value"`
}

type responseError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type protocolError struct {
	Code      string
	Message   string
	Status    int
	Retryable bool
}

func (e *protocolError) Error() string { return e.Code }

func decodeRequest(body []byte) (*outerRequest, *protocolError) {
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return nil, invalidRequest("INVALID_JSON", "Request body must be valid JSON.")
	}
	if err := validateJSONShape(body); err != nil {
		return nil, invalidRequest("INVALID_REQUEST", "Request body does not match the gateway protocol.")
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var envelope outerRequest
	if err := decoder.Decode(&envelope); err != nil {
		return nil, invalidRequest("INVALID_REQUEST", "Request body does not match the gateway protocol.")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, invalidRequest("INVALID_JSON", "Request body must contain one JSON value.")
	}
	if err := validateOuterRequest(&envelope); err != nil {
		return nil, err
	}
	return &envelope, nil
}

func validateJSONShape(body []byte) error {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(body, &outer); err != nil {
		return err
	}
	if !hasExactKeys(outer, "version", "connectionString", "request") {
		return errors.New("invalid outer request fields")
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(outer["request"], &request); err != nil {
		return err
	}
	if !hasOnlyKeys(request, "version", "operation", "alias", "sql", "parameters", "options") ||
		!hasKeys(request, "version", "operation", "alias") {
		return errors.New("invalid request fields")
	}
	if raw, ok := request["parameters"]; ok {
		var parameters []json.RawMessage
		if err := json.Unmarshal(raw, &parameters); err != nil {
			return err
		}
		for _, rawParameter := range parameters {
			var parameter map[string]json.RawMessage
			if err := json.Unmarshal(rawParameter, &parameter); err != nil {
				return err
			}
			if !hasOnlyKeys(parameter, "name", "ordinal", "type", "value") ||
				!hasKeys(parameter, "ordinal", "type", "value") {
				return errors.New("invalid parameter fields")
			}
		}
	}
	if raw, ok := request["options"]; ok {
		var options map[string]json.RawMessage
		if err := json.Unmarshal(raw, &options); err != nil {
			return err
		}
		if !hasOnlyKeys(options, "maxRows", "timeoutMs") {
			return errors.New("invalid option fields")
		}
	}
	return nil
}

func validateOuterRequest(envelope *outerRequest) *protocolError {
	if envelope.Version != protocolVersion {
		return invalidRequest("UNSUPPORTED_VERSION", "Only gateway protocol version 1 is supported.")
	}
	if envelope.ConnectionString == "" || len(envelope.ConnectionString) > MaxConnectionBytes ||
		strings.IndexFunc(envelope.ConnectionString, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return invalidRequest("INVALID_CONNECTION", "A valid connection string is required.")
	}
	request := &envelope.Request
	if request.Version != protocolVersion {
		return invalidRequest("UNSUPPORTED_VERSION", "Only request protocol version 1 is supported.")
	}
	if !aliasPattern.MatchString(request.Alias) {
		return invalidRequest("INVALID_ALIAS", "Connection alias is invalid.")
	}
	if request.Operation != "ping" && request.Operation != "query" && request.Operation != "exec" {
		return invalidRequest("INVALID_OPERATION", "Operation must be ping, query, or exec.")
	}
	if request.Operation == "ping" {
		if request.SQL != nil || request.Parameters != nil || request.Options != nil {
			return invalidRequest("INVALID_PING", "Ping does not accept SQL, parameters, or options.")
		}
		return nil
	}
	if request.SQL == nil || strings.TrimSpace(*request.SQL) == "" || strings.ContainsRune(*request.SQL, '\x00') || len(*request.SQL) > MaxSQLBytes {
		return invalidRequest("INVALID_SQL", "A non-empty SQL statement within the size limit is required.")
	}
	if request.Parameters != nil {
		if len(*request.Parameters) > MaxParameterCount {
			return invalidRequest("TOO_MANY_PARAMETERS", "SQL parameter count exceeds the limit.")
		}
		for index := range *request.Parameters {
			parameter := &(*request.Parameters)[index]
			if parameter.Ordinal != index+1 || !parameterNamePattern.MatchString(parameter.Name) {
				return invalidRequest("INVALID_PARAMETER", "SQL parameter metadata is invalid.")
			}
			if _, err := parameter.driverValue(); err != nil {
				return err
			}
		}
	}
	if request.Options != nil {
		if request.Options.MaxRows != nil && (*request.Options.MaxRows < 1 || *request.Options.MaxRows > MaxRows) {
			return invalidRequest("INVALID_MAX_ROWS", "maxRows must be between 1 and 10000.")
		}
		if request.Options.TimeoutMS != nil && (*request.Options.TimeoutMS < 100 || *request.Options.TimeoutMS > int(MaxTimeout/time.Millisecond)) {
			return invalidRequest("INVALID_TIMEOUT", "timeoutMs must be between 100 and 180000.")
		}
	}
	return nil
}

func (r requestEnvelope) maxRows() int {
	if r.Options != nil && r.Options.MaxRows != nil {
		return *r.Options.MaxRows
	}
	return defaultMaxRows
}

func (r requestEnvelope) timeout() time.Duration {
	if r.Options != nil && r.Options.TimeoutMS != nil {
		return time.Duration(*r.Options.TimeoutMS) * time.Millisecond
	}
	return defaultTimeout
}

func (r requestEnvelope) driverArguments() ([]any, *protocolError) {
	if r.Parameters == nil {
		return nil, nil
	}
	arguments := make([]any, len(*r.Parameters))
	for index, parameter := range *r.Parameters {
		value, err := parameter.driverValue()
		if err != nil {
			return nil, err
		}
		if parameter.Name != "" {
			arguments[index] = sql.Named(parameter.Name, value)
		} else {
			arguments[index] = value
		}
	}
	return arguments, nil
}

func (p requestParameter) driverValue() (any, *protocolError) {
	invalid := func() (any, *protocolError) {
		return nil, invalidRequest("INVALID_PARAMETER_VALUE", "SQL parameter value is invalid.")
	}
	switch p.Type {
	case "null":
		if !bytes.Equal(bytes.TrimSpace(p.Value), []byte("null")) {
			return invalid()
		}
		return nil, nil
	case "bool":
		var value bool
		if err := json.Unmarshal(p.Value, &value); err != nil {
			return invalid()
		}
		return value, nil
	case "int64":
		value, ok := decodeBoundedString(p.Value, 32)
		if !ok || !int64Pattern.MatchString(value) {
			return invalid()
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return invalid()
		}
		return parsed, nil
	case "float64":
		value, ok := decodeBoundedString(p.Value, 128)
		if !ok || !float64Pattern.MatchString(value) {
			return invalid()
		}
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return invalid()
		}
		return parsed, nil
	case "string":
		value, ok := decodeBoundedString(p.Value, MaxParameterValueSize)
		if !ok || len(value) > MaxParameterValueSize {
			return invalid()
		}
		return value, nil
	case "bytes":
		value, ok := decodeBoundedString(p.Value, MaxRequestBytes)
		if !ok {
			return invalid()
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(value)
		if err != nil || len(decoded) > MaxParameterValueSize {
			return invalid()
		}
		return decoded, nil
	case "time":
		value, ok := decodeBoundedString(p.Value, 64)
		if !ok {
			return invalid()
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return invalid()
		}
		return parsed, nil
	default:
		return invalid()
	}
}

func decodeBoundedString(raw json.RawMessage, limit int64) (string, bool) {
	if int64(len(raw)) > limit+2 {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || int64(len(value)) > limit {
		return "", false
	}
	return value, true
}

func invalidRequest(code, message string) *protocolError {
	return &protocolError{Code: code, Message: message, Status: 400}
}

func hasExactKeys(value map[string]json.RawMessage, keys ...string) bool {
	return len(value) == len(keys) && hasKeys(value, keys...)
}

func hasOnlyKeys(value map[string]json.RawMessage, keys ...string) bool {
	allowed := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		allowed[key] = struct{}{}
	}
	for key := range value {
		if _, ok := allowed[key]; !ok {
			return false
		}
	}
	return true
}

func hasKeys(value map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		if _, ok := value[key]; !ok {
			return false
		}
	}
	return true
}

func rejectDuplicateJSONKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := walkJSONValue(decoder, 0); err != nil {
		return err
	}
	return ensureJSONEOF(decoder)
}

func walkJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("JSON nesting exceeds the limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			keys[key] = struct{}{}
			if err := walkJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	_, err := decoder.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}
