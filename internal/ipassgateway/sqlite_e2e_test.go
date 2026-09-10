//go:build moderncsqlite && !no_moderncsqlite

package ipassgateway

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xo/dburl"
	"github.com/xo/usql/drivers/ipass/transport"
	_ "github.com/xo/usql/internal"
)

// The opaque URL keeps :memory: out of the authority parser, whose handling
// differs between Go versions. Its DSN must stay SQLite's in-memory sentinel.
const sqliteMemoryConnectionString = "moderncsqlite::memory:"

func TestModerncSQLiteMemoryConnectionString(t *testing.T) {
	parsed, err := dburl.Parse(sqliteMemoryConnectionString)
	if err != nil {
		t.Fatalf("parse in-memory SQLite connection: %v", err)
	}
	if parsed.Driver != "moderncsqlite" || parsed.DSN != ":memory:" || parsed.Host != "" {
		t.Fatalf("unexpected in-memory SQLite connection: driver=%q dsn=%q host=%q", parsed.Driver, parsed.DSN, parsed.Host)
	}
}

func TestModerncSQLiteEndToEnd(t *testing.T) {
	opener, err := NewDBURLOpener([]string{"moderncsqlite"})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	handler, err := NewHandler(Config{
		Opener: opener,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	ping := executeSQLiteRequest(t, handler, map[string]any{
		"version":          1,
		"connectionString": sqliteMemoryConnectionString,
		"request": map[string]any{
			"version": 1, "operation": "ping", "alias": "sqlite-e2e",
		},
	})
	if ping["ok"] != true {
		t.Fatalf("ping failed: %#v", ping)
	}

	exec := executeSQLiteRequest(t, handler, map[string]any{
		"version":          1,
		"connectionString": sqliteMemoryConnectionString,
		"request": map[string]any{
			"version": 1, "operation": "exec", "alias": "sqlite-e2e",
			"sql":        "CREATE TABLE sample (id INTEGER PRIMARY KEY, label TEXT)",
			"parameters": []any{},
			"options":    map[string]any{"timeoutMs": 120000},
		},
	})
	if exec["ok"] != true {
		t.Fatalf("exec failed: %#v", exec)
	}

	query := executeSQLiteRequest(t, handler, map[string]any{
		"version":          1,
		"connectionString": sqliteMemoryConnectionString,
		"request": map[string]any{
			"version": 1, "operation": "query", "alias": "sqlite-e2e",
			"sql": "SELECT CAST(? AS INTEGER) AS id, ? AS label, ? AS payload",
			"parameters": []any{
				map[string]any{"name": "", "ordinal": 1, "type": "int64", "value": "42"},
				map[string]any{"name": "", "ordinal": 2, "type": "string", "value": "hello"},
				map[string]any{"name": "", "ordinal": 3, "type": "bytes", "value": base64.StdEncoding.EncodeToString([]byte{1, 2, 3})},
			},
			"options": map[string]any{"maxRows": 1000, "timeoutMs": 120000},
		},
	})
	if query["ok"] != true {
		t.Fatalf("query failed: %#v", query)
	}
	rows, ok := query["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("unexpected rows: %#v", query["rows"])
	}
	cells, ok := rows[0].([]any)
	if !ok || len(cells) != 3 {
		t.Fatalf("unexpected cells: %#v", rows[0])
	}
	first := cells[0].(map[string]any)
	if first["type"] != "int64" || first["value"] != "42" {
		t.Fatalf("unexpected integer cell: %#v", first)
	}
	third := cells[2].(map[string]any)
	if third["type"] != "bytes" || third["value"] != "AQID" {
		t.Fatalf("unexpected bytes cell: %#v", third)
	}

	for _, forbidden := range []string{sqliteMemoryConnectionString, "CREATE TABLE sample", "SELECT CAST"} {
		if bytes.Contains(logs.Bytes(), []byte(forbidden)) {
			t.Fatalf("gateway logs exposed %q", forbidden)
		}
	}
}

func TestTransportToModerncSQLiteParameterCompatibility(t *testing.T) {
	opener, err := NewDBURLOpener([]string{"moderncsqlite"})
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := NewHandler(Config{Opener: opener})
	if err != nil {
		t.Fatal(err)
	}
	const capability = "0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/runs/"+capability+"/db/v1/execute" {
			t.Errorf("unexpected transport route: %s %s", request.Method, request.URL.Path)
			http.Error(writer, "unexpected transport route", http.StatusBadRequest)
			return
		}
		var inner json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&inner); err != nil {
			t.Errorf("decode transport request: %v", err)
			http.Error(writer, "invalid transport request", http.StatusBadRequest)
			return
		}
		// Add only the Action's outer envelope. Preserve the client's parameter
		// fields exactly so optional-field mismatches reach the real gateway.
		body, err := json.Marshal(map[string]any{
			"version":          1,
			"connectionString": sqliteMemoryConnectionString,
			"request":          inner,
		})
		if err != nil {
			t.Errorf("encode gateway request: %v", err)
			http.Error(writer, "invalid gateway request", http.StatusInternalServerError)
			return
		}
		forwarded := httptest.NewRequest(http.MethodPost, ExecutePath, bytes.NewReader(body)).WithContext(request.Context())
		forwarded.Header.Set("Content-Type", "application/json")
		gateway.ServeHTTP(writer, forwarded)
	}))
	defer server.Close()
	t.Setenv(transport.BaseURLEnv, server.URL+"/v1/runs/"+capability+"/db")
	connector, err := (&transport.Driver{}).OpenConnector("sqlite-transport")
	if err != nil {
		t.Fatal(err)
	}
	database := sql.OpenDB(connector)
	defer database.Close()
	if err := database.PingContext(t.Context()); err != nil {
		t.Fatalf("transport ping: %v", err)
	}
	for _, test := range []struct {
		name      string
		statement string
		arguments []any
	}{
		{"positional", "SELECT CAST(? AS INTEGER) AS id, ? AS label", []any{42, "sample"}},
		{"named", "SELECT CAST(@id AS INTEGER) AS id, @label AS label", []any{sql.Named("id", 42), sql.Named("label", "sample")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var id int64
			var label string
			if err := database.QueryRowContext(t.Context(), test.statement, test.arguments...).Scan(&id, &label); err != nil {
				t.Fatalf("transport query: %v", err)
			}
			if id != 42 || label != "sample" {
				t.Fatalf("unexpected row: %d %q", id, label)
			}
		})
	}
}

func executeSQLiteRequest(t *testing.T, handler http.Handler, requestBody map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, ExecutePath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	result := response.Result()
	defer result.Body.Close()
	responseBody, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("got status %d: %s", result.StatusCode, responseBody)
	}
	var decoded map[string]any
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}
