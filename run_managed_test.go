//go:build managed

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/xo/usql/drivers"
	"github.com/xo/usql/drivers/ipass/transport"
)

func TestManagedCommandConnectsFromEnvironment(t *testing.T) {
	for _, test := range []struct {
		name  string
		dsn   string
		alias string
	}{
		{"implicit default", "", "default"},
		{"explicit alias", "ipass://orders", "orders"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var operations []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Operation string `json:"operation"`
					Alias     string `json:"alias"`
					SQL       string `json:"sql"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if request.Alias != test.alias {
					t.Errorf("alias = %q, want %q", request.Alias, test.alias)
				}
				operations = append(operations, request.Operation)
				w.Header().Set("Content-Type", "application/json")
				switch request.Operation {
				case "ping":
					_, _ = w.Write([]byte(`{"version":1,"ok":true}`))
				case "query":
					if strings.TrimSpace(request.SQL) != "SELECT 1 AS value" {
						t.Errorf("SQL = %q", request.SQL)
					}
					_, _ = w.Write([]byte(`{"version":1,"ok":true,"columns":[{"name":"value","databaseType":"BIGINT"}],"rows":[[{"type":"int64","value":"1"}]]}`))
				default:
					t.Errorf("unexpected operation: %s", request.Operation)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			t.Setenv(transport.BaseURLEnv, server.URL+"/v1/runs/0123456789abcdef0123456789abcdef/db")
			args := []string{"usql", "-X", "-J", "-c", "SELECT 1 AS value"}
			if test.dsn != "" {
				args = append(args, test.dsn)
			}
			if err := New(args).ExecuteContext(t.Context()); err != nil {
				t.Fatalf("execute command: %v", err)
			}
			if !reflect.DeepEqual(operations, []string{"ping", "query"}) {
				t.Fatalf("operations = %v, want ping then query", operations)
			}
		})
	}
}

func TestManagedCommandRejectsInvalidEnvironmentURL(t *testing.T) {
	t.Setenv(transport.BaseURLEnv, "http://example.com/v1/runs/0123456789abcdef0123456789abcdef/db")
	err := New([]string{"usql", "-X", "-c", "SELECT 1"}).ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "cleartext HTTP on loopback") {
		t.Fatalf("expected URL validation error, got %v", err)
	}
}

func TestManagedEnvironmentDoesNotOverrideExplicitDriver(t *testing.T) {
	t.Setenv(transport.BaseURLEnv, "http://127.0.0.1:1/v1/runs/0123456789abcdef0123456789abcdef/db")
	err := New([]string{"usql", "-X", "-c", "SELECT 1", "postgres://localhost/test"}).ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "disabled in the managed edition") {
		t.Fatalf("expected direct driver rejection, got %v", err)
	}
}

func TestManagedBuildRegistersOnlyIPassDriver(t *testing.T) {
	available := drivers.Available()
	names := make([]string, 0, len(available))
	for name := range available {
		names = append(names, name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"ipass"}) {
		t.Fatalf("managed build registered drivers %v, want only ipass", names)
	}
}

func TestManagedBuildRejectsConfigFlag(t *testing.T) {
	err := New([]string{"usql", "--config", "shared.yaml"}).ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "configuration files") {
		t.Fatalf("expected configuration file error, got %v", err)
	}
}

func TestManagedBuildRejectsShellMetaCommand(t *testing.T) {
	err := New([]string{"usql", "--command", `\! whoami`}).ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "disabled in the managed edition") {
		t.Fatalf("expected shell meta-command error, got %v", err)
	}
}

func TestValidateManagedArgs(t *testing.T) {
	tests := []struct {
		name string
		args Args
	}{
		{"input file", Args{CommandOrFiles: []CommandOrFile{{Command: false, Value: "input.sql"}}}},
		{"output file", Args{Out: "output.txt"}},
		{"password prompt", Args{ForcePassword: true}},
		{"transaction", Args{SingleTransaction: true}},
		{"connection variable", Args{Cvars: []string{"prod=postgres://secret"}}},
		{"connection config", Args{Connections: map[string]interface{}{"prod": "postgres://secret"}}},
		{"application variable", Args{Vars: []string{"PAGER=cmd"}}},
		{"pager", Args{Pvars: []string{"pager=always"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateManagedArgs(&test.args); err == nil {
				t.Fatal("expected managed argument policy error")
			}
		})
	}

	allowed := Args{
		CommandOrFiles: []CommandOrFile{{Command: true, Value: "select 1"}},
		Vars:           []string{"QUIET=on"},
		Pvars:          []string{"format=json"},
	}
	if err := validateManagedArgs(&allowed); err != nil {
		t.Fatalf("expected safe arguments to be allowed: %v", err)
	}
}
