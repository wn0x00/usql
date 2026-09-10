package transport

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPingQueryAndExec(t *testing.T) {
	const capability = "0123456789abcdef0123456789abcdef"
	fixedTime := time.Date(2026, time.September, 9, 12, 30, 45, 123000000, time.FixedZone("CST", 8*60*60))
	var operations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s", request.Method)
		}
		if request.URL.Path != "/v1/runs/"+capability+"/db/v1/execute" {
			t.Errorf("path = %s", request.URL.Path)
		}
		if request.URL.RawQuery != "" {
			t.Errorf("unexpected query = %s", request.URL.RawQuery)
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %s", request.Header.Get("Content-Type"))
		}
		var envelope requestEnvelope
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		operations = append(operations, envelope.Operation)
		if envelope.Version != protocolVersion || envelope.Alias != "orders" {
			t.Errorf("unexpected envelope: %+v", envelope)
		}
		w.Header().Set("Content-Type", "application/json")
		switch envelope.Operation {
		case "ping":
			_, _ = w.Write([]byte(`{"version":1,"ok":true}`))
		case "query":
			if len(envelope.Parameters) != 2 || envelope.Parameters[0].Type != "int64" || envelope.Parameters[0].Value != "7" {
				t.Errorf("unexpected query parameters: %#v", envelope.Parameters)
			}
			if envelope.Options == nil || envelope.Options.MaxRows != maxRows || envelope.Options.TimeoutMS != 120000 {
				t.Errorf("unexpected query options: %#v", envelope.Options)
			}
			response := map[string]interface{}{
				"version": 1,
				"ok":      true,
				"columns": []map[string]interface{}{
					{"name": "id", "databaseType": "BIGINT", "nullable": false},
					{"name": "name", "databaseType": "VARCHAR", "nullable": true, "length": 200},
					{"name": "created_at", "databaseType": "TIMESTAMPTZ"},
					{"name": "payload", "databaseType": "BYTEA"},
				},
				"rows": [][]map[string]interface{}{{
					{"type": "int64", "value": "9223372036854775807"},
					{"type": "string", "value": "sample"},
					{"type": "time", "value": fixedTime.Format(time.RFC3339Nano)},
					{"type": "bytes", "value": base64.StdEncoding.EncodeToString([]byte{0, 1, 2})},
				}},
				"affectedRows": "0",
				"truncated":    false,
			}
			_ = json.NewEncoder(w).Encode(response)
		case "exec":
			_, _ = w.Write([]byte(`{"version":1,"ok":true,"affectedRows":"3","lastInsertId":"42"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	t.Setenv(BaseURLEnv, server.URL+"/v1/runs/"+capability+"/db")
	connector, err := (&Driver{}).OpenConnector("orders")
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	resultRows, err := db.QueryContext(ctx, "SELECT id, name, created_at, payload FROM orders WHERE id = ? AND active = ?", 7, true)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer resultRows.Close()
	if !resultRows.Next() {
		t.Fatalf("expected a row: %v", resultRows.Err())
	}
	var id int64
	var name string
	var createdAt time.Time
	var payload []byte
	if err := resultRows.Scan(&id, &name, &createdAt, &payload); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if id != int64(9223372036854775807) || name != "sample" || !createdAt.Equal(fixedTime) || !reflect.DeepEqual(payload, []byte{0, 1, 2}) {
		t.Fatalf("unexpected row: %d %q %s %v", id, name, createdAt, payload)
	}

	result, err := db.ExecContext(ctx, "UPDATE orders SET active = ?", false)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 3 {
		t.Fatalf("rows affected = %d, %v", affected, err)
	}
	if inserted, err := result.LastInsertId(); err != nil || inserted != 42 {
		t.Fatalf("last insert id = %d, %v", inserted, err)
	}
	if !reflect.DeepEqual(operations, []string{"ping", "query", "exec"}) {
		t.Fatalf("operations = %#v", operations)
	}
}

func TestRemoteErrorIsTypedAndBounded(t *testing.T) {
	const capability = "0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"version":1,"ok":false,"error":{"code":"SQL_DENIED","message":"write is not allowed","retryable":false}}`))
	}))
	defer server.Close()
	t.Setenv(BaseURLEnv, server.URL+"/v1/runs/"+capability+"/db")

	connector, err := (&Driver{}).OpenConnector("readonly")
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	_, err = db.ExecContext(context.Background(), "DELETE FROM orders")
	var remote *RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("expected RemoteError, got %T: %v", err, err)
	}
	if remote.Code != "SQL_DENIED" || remote.Status != http.StatusForbidden {
		t.Fatalf("unexpected remote error: %+v", remote)
	}
}

func TestAdapterBaseURLValidation(t *testing.T) {
	const capability = "0123456789abcdef0123456789abcdef"
	for _, raw := range []string{
		"",
		"ftp://example.com/path",
		"http://example.com/path",
		"https://user:pass@example.com/path",
		"https://example.com/path?token=secret",
		"https://example.com",
		"https://example.com/v1/runs/../db",
		"https://example.com/v1/runs/capability/other",
		"https://example.com/v1/runs/too-short/db",
		"https://example.com/v1/runs/0123456789abcdef0123456789abcde./db",
		"https://example.com/v1/runs/0123456789abcdef0123456789abcdef%2Fother/db",
		"https://example.com/proxy/%2e%2e/v1/runs/" + capability + "/db",
	} {
		if _, err := adapterBaseURL(raw); err == nil {
			t.Errorf("expected %q to be rejected", raw)
		}
	}
	for _, raw := range []string{
		"http://127.0.0.1:18766/v1/runs/" + capability + "/db",
		"http://[::1]:18766/v1/runs/" + capability + "/db",
		"http://localhost:18766/v1/runs/" + capability + "/db",
		"https://adapter.example.com/v1/runs/" + capability + "/db",
		"https://adapter.example.com/proxy/sql/v1/runs/" + capability + "/db",
	} {
		if _, err := adapterBaseURL(raw); err != nil {
			t.Errorf("expected %q to be allowed: %v", raw, err)
		}
	}
}

func TestConnectionAliasValidation(t *testing.T) {
	t.Setenv(BaseURLEnv, "http://127.0.0.1:18766/v1/runs/0123456789abcdef0123456789abcdef/db")
	for _, alias := range []string{"", "../other", "a/b", strings.Repeat("a", 65), "中文"} {
		if _, err := (&Driver{}).OpenConnector(alias); err == nil {
			t.Errorf("expected alias %q to be rejected", alias)
		}
	}
	for _, alias := range []string{"default", "orders-prod", "warehouse_01", "tenant.db"} {
		if _, err := (&Driver{}).OpenConnector(alias); err != nil {
			t.Errorf("expected alias %q to be allowed: %v", alias, err)
		}
	}
}

func TestTypedValueDecoderRejectsLossyInt(t *testing.T) {
	_, err := decodeCell(responseCell{Type: "int64", Value: json.RawMessage(`9007199254740993`)})
	if err == nil {
		t.Fatal("expected an unquoted int64 to be rejected")
	}
}

func TestTransportErrorDoesNotExposeRunCapability(t *testing.T) {
	const capability = "top_secret_run_capability_12345678"
	baseURL := "https://adapter.example.com/v1/runs/" + capability + "/db"
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: request.Method, URL: request.URL.String(), Err: errors.New("dial failed")}
	})}
	connector := &connector{alias: "orders", baseURL: baseURL, client: client}

	_, err := connector.call(context.Background(), requestEnvelope{Version: protocolVersion, Operation: "ping", Alias: "orders"})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), capability) || strings.Contains(err.Error(), baseURL) {
		t.Fatalf("transport error exposed the run capability: %v", err)
	}
}

func TestOversizedRequestIsRejectedBeforeTransport(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected transport call")
	})}
	connector := &connector{
		alias:   "orders",
		baseURL: "https://adapter.example.com/v1/runs/0123456789abcdef0123456789abcdef/db",
		client:  client,
	}

	_, err := connector.call(context.Background(), requestEnvelope{
		Version:   protocolVersion,
		Operation: "query",
		Alias:     "orders",
		SQL:       strings.Repeat("x", maxRequestBytes),
	})
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("expected request size error, got %v", err)
	}
	if called {
		t.Fatal("oversized request reached the HTTP transport")
	}
}

func TestStatementLimitsAreCheckedBeforeEncoding(t *testing.T) {
	if err := validateStatement(strings.Repeat("x", maxSQLBytes+1), nil); err == nil {
		t.Fatal("expected oversized SQL to be rejected")
	}
	parameters := make([]driver.NamedValue, maxParameters+1)
	if err := validateStatement("select 1", parameters); err == nil {
		t.Fatal("expected excessive SQL parameters to be rejected")
	}
	if err := validateStatement(strings.Repeat("x", maxSQLBytes), make([]driver.NamedValue, maxParameters)); err != nil {
		t.Fatalf("expected boundary-sized statement to be allowed: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
