package ipassgateway

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type openerFunc func(context.Context, string) (*sql.DB, error)

func (f openerFunc) Open(ctx context.Context, connectionString string) (*sql.DB, error) {
	return f(ctx, connectionString)
}

func TestRequestValidationIsStrict(t *testing.T) {
	t.Parallel()
	handler, err := NewHandler(Config{Opener: openerFunc(func(context.Context, string) (*sql.DB, error) {
		t.Fatal("invalid request reached the database opener")
		return nil, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		method      string
		target      string
		contentType string
		body        string
		status      int
	}{
		{"path", http.MethodPost, "/v1/query", "application/json", `{}`, http.StatusNotFound},
		{"query string", http.MethodPost, ExecutePath + "?debug=true", "application/json", `{}`, http.StatusBadRequest},
		{"method", http.MethodGet, ExecutePath, "application/json", `{}`, http.StatusMethodNotAllowed},
		{"content type", http.MethodPost, ExecutePath, "text/plain", `{}`, http.StatusUnsupportedMediaType},
		{"unknown field", http.MethodPost, ExecutePath, "application/json", `{"version":1,"connectionString":"postgres://host/db","request":{"version":1,"operation":"ping","alias":"test","extra":true}}`, http.StatusBadRequest},
		{"duplicate field", http.MethodPost, ExecutePath, "application/json", `{"version":1,"connectionString":"postgres://host/db","request":{"version":1,"operation":"ping","alias":"test","alias":"other"}}`, http.StatusBadRequest},
		{"ping fields", http.MethodPost, ExecutePath, "application/json", `{"version":1,"connectionString":"postgres://host/db","request":{"version":1,"operation":"ping","alias":"test","sql":"SELECT 1"}}`, http.StatusBadRequest},
		{"float number", http.MethodPost, ExecutePath, "application/json", `{"version":1,"connectionString":"postgres://host/db","request":{"version":1,"operation":"query","alias":"test","sql":"SELECT ?","parameters":[{"name":"","ordinal":1,"type":"float64","value":1.5}]}}`, http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("got status %d, want %d; body=%s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestRequestBodyLimit(t *testing.T) {
	t.Parallel()
	handler, err := NewHandler(Config{Opener: openerFunc(func(context.Context, string) (*sql.DB, error) {
		t.Fatal("oversized request reached the database opener")
		return nil, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, ExecutePath, io.LimitReader(strings.NewReader(strings.Repeat("x", int(MaxRequestBytes)+1)), MaxRequestBytes+1))
	request.Header.Set("Content-Type", "application/json")
	request.ContentLength = -1
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got status %d, want 413", response.Code)
	}
}

func TestParameterShapeAllowsOptionalName(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		parameter map[string]any
		valid     bool
	}{
		{"positional", map[string]any{"ordinal": 1, "type": "int64", "value": "42"}, true},
		{"empty name", map[string]any{"name": "", "ordinal": 1, "type": "int64", "value": "42"}, true},
		{"named", map[string]any{"name": "id", "ordinal": 1, "type": "int64", "value": "42"}, true},
		{"unknown field", map[string]any{"ordinal": 1, "type": "int64", "value": "42", "extra": true}, false},
		{"missing ordinal", map[string]any{"type": "int64", "value": "42"}, false},
		{"missing type", map[string]any{"ordinal": 1, "value": "42"}, false},
		{"missing value", map[string]any{"ordinal": 1, "type": "null"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"version":          1,
				"connectionString": "moderncsqlite://:memory:",
				"request": map[string]any{
					"version": 1, "operation": "query", "alias": "parameter-shape",
					"sql": "SELECT ?", "parameters": []any{test.parameter},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, protocolErr := decodeRequest(body)
			if (protocolErr == nil) != test.valid {
				t.Fatalf("valid = %v, want %v; error = %v", protocolErr == nil, test.valid, protocolErr)
			}
		})
	}
}

func TestErrorsAndLogsDoNotExposeSecrets(t *testing.T) {
	t.Parallel()
	const (
		connectionString    = "postgres://secret-user:secret-password@private.invalid/private-db"
		statement           = "SELECT 'secret-sql-literal'"
		authorizationSecret = "ignored-authorization-secret"
	)
	var logs bytes.Buffer
	handler, err := NewHandler(Config{
		Opener: openerFunc(func(_ context.Context, value string) (*sql.DB, error) {
			if value != connectionString {
				t.Fatalf("connection string was not passed intact")
			}
			return nil, errors.New(connectionString + " " + statement + " " + authorizationSecret)
		}),
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"connectionString":"` + connectionString + `","request":{"version":1,"operation":"query","alias":"private-alias","sql":"` + statement + `"}}`
	request := httptest.NewRequest(http.MethodPost, ExecutePath, strings.NewReader(body))
	request.Header.Set("Authorization", authorizationSecret)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("got status %d, want 502; body=%s", response.Code, response.Body.String())
	}
	combined := response.Body.String() + logs.String()
	for _, secret := range []string{connectionString, "secret-user", "secret-password", statement, "secret-sql-literal", authorizationSecret} {
		if strings.Contains(combined, secret) {
			t.Fatalf("secret %q leaked in response or logs", secret)
		}
	}
}

func TestLoopbackListenerDetection(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"127.0.0.1:18768", "[::1]:18768", "localhost:18768"} {
		if !IsLoopbackListenAddress(address) {
			t.Errorf("expected %q to be loopback", address)
		}
	}
	for _, address := range []string{":18768", "0.0.0.0:18768", "example.com:18768", "127.0.0.1", "127.0.0.1:0"} {
		if IsLoopbackListenAddress(address) {
			t.Errorf("expected %q not to be an explicit loopback listener", address)
		}
	}
}

func TestUnsupportedDriverCannotEnterAllowlist(t *testing.T) {
	t.Parallel()
	_, err := NewDBURLOpener([]string{"oracle"})
	if !errors.Is(err, ErrDriverNotAllowed) {
		t.Fatalf("got %v, want ErrDriverNotAllowed", err)
	}
}
