# usql iPaaS database gateway

`usql-gateway` is the database execution service behind the Yingdao iPaaS
Action. It is intentionally separate from the CLI Adapter:

```text
usql-ipass CLI -> local Adapter -> iPaaS Action -> HTTPS gateway -> database
```

The gateway exposes exactly one route, `POST /v1/execute`. The Action sends the
connection string in the JSON request body. The gateway has no application-level
authentication mechanism and does not inspect `Authorization`; the connection
string, SQL text, parameter values, headers, and request body are never logged.

## Build

The managed build includes only PostgreSQL, MySQL, SQL Server, and the pure-Go
ModernC SQLite driver:

```powershell
./cmd/usql-gateway/build.ps1
```

The SQLite driver exists for local verification. Do not put `moderncsqlite` in
the production allowlist unless file-backed database access is intentional.

## Configuration

| Environment variable | Required | Purpose |
| --- | --- | --- |
| `USQL_GATEWAY_LISTEN_ADDR` | No | TCP listener; defaults to `127.0.0.1:18768`. |
| `USQL_GATEWAY_ALLOWED_DRIVERS` | Yes | Comma-separated canonical names: `postgres`, `mysql`, `sqlserver`, `moderncsqlite`. |
| `USQL_GATEWAY_ALLOW_REMOTE` | For non-loopback | Must be exactly `true` to acknowledge a non-loopback listener. This is only a startup guard, not authentication. |
| `USQL_GATEWAY_TLS_CERT_FILE` | With key | Optional native TLS certificate. |
| `USQL_GATEWAY_TLS_KEY_FILE` | With certificate | Optional native TLS private key. |

## Deployment boundary

The safe default is a loopback-only listener. Setting
`USQL_GATEWAY_ALLOW_REMOTE=true` merely disables that startup guard; it does not
add authentication, authorization, encryption, or tenant isolation. Never
expose the gateway directly to the public Internet.

For a cross-host deployment, put it on a private network and enforce source IP
allowlists at the firewall or load balancer. Prefer a trusted reverse proxy or
service mesh that requires mTLS, and expose the gateway only through HTTPS.
Restrict database firewall rules so that only the gateway can reach database
ports. The native TLS options encrypt the connection but do not by themselves
authenticate clients.

Local SQLite smoke test:

```powershell
$env:USQL_GATEWAY_ALLOWED_DRIVERS = "moderncsqlite"
./bin/usql-gateway.exe
```

Example request (the connection string is shown only for local development):

```powershell
$body = @{
    version = 1
    connectionString = "moderncsqlite://:memory:"
    request = @{
        version = 1
        operation = "query"
        alias = "local-test"
        sql = "SELECT 1 AS id"
        parameters = @()
        options = @{ maxRows = 1000; timeoutMs = 120000 }
    }
} | ConvertTo-Json -Depth 8
Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:18768/v1/execute" -ContentType "application/json" -Body $body
```

## Tests

```powershell
& "C:\Program Files\Go\bin\go.exe" test -tags "no_base moderncsqlite" ./internal/ipassgateway ./cmd/usql-gateway
```

The tagged tests execute real `ping`, `query`, and `exec` operations through
ModernC SQLite. The default tests also cover strict JSON validation, size
limits, loopback-by-default startup policy, driver isolation, and log
redaction.
