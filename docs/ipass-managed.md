# Managed Yingdao iPaaS edition

This fork can build a constrained `usql` executable that carries only the
`ipass` SQL driver. The client never receives a database DSN, password, API
key, or iPaaS `authId`. The sandbox injects one short-lived run-capability URL
and the Adapter resolves the current run to the server-side credential.

## Build

Use the dedicated PowerShell script on Windows or PowerShell 7 on Linux/macOS:

```powershell
./scripts/build-managed.ps1
```

To use a Go executable outside `PATH`:

```powershell
./scripts/build-managed.ps1 `
  -GoExecutable 'C:\path\to\go.exe'
```

The script runs the managed tests, builds with the fixed tags
`managed no_base ipass`, verifies that the iPaaS driver is present and a
direct PostgreSQL driver is absent, and writes `bin/usql.exe` on Windows or
`bin/usql` on Linux. It also writes a sibling `usql(.exe).sha256` manifest in
the exact form `<hex>  <filename>`. Configure the manifest hex as
`CLI_ADAPTER_USQL_SHA256` together with the Adapter's explicit executable
path; this prevents a PATH substitution or an unapproved binary from running.
`CGO_ENABLED=0` is forced. The workflow in
`.github/workflows/managed-ipass.yml` performs the same build for Windows and
Linux and uploads both files as artifacts. The separate
`.github/workflows/publish-npm.yml` builds and verifies the five npm targets:
Windows x64, Linux x64/arm64, and macOS x64/arm64. See
[npm distribution](npm-release.md) for installation and OIDC publishing.

## Invoke

The sandbox must inject an identity-bound URL for this process only:

```powershell
$env:USQL_IPASS_BASE_URL = 'http://127.0.0.1:18766/v1/runs/<capability>/db'
./bin/usql.exe -X -J -c 'select id, name from orders'
```

When the managed executable detects a non-empty `USQL_IPASS_BASE_URL` and
no positional DSN, it automatically connects using `ipass://default`. There
is no `--ipass` flag. An explicit `ipass://orders` remains supported as a
display/audit alias and takes precedence over the default. The URL is still
validated by the driver before any request; an invalid URL fails without
falling back to another connection. With no URL configured, the executable
retains its disconnected startup behaviour. Community builds are unchanged.

For a non-loopback Adapter the URL must use HTTPS. Its path must end with
`/v1/runs/<capability>/db`, where the URL-safe capability is 32-128 ASCII
letters, digits, underscores, or hyphens. An HTTPS reverse-proxy path may
precede that suffix. Percent-encoded path segments, dot segments, user
information, queries, and fragments are rejected. Do not put the URL in a DSN,
config file, log, or command history. `orders` is a non-secret ASCII
display/audit alias; it does not select an authorization, profile, connection
string, or tenant.

The client always sends exactly:

```text
POST ${USQL_IPASS_BASE_URL}/v1/execute
Content-Type: application/json
```

It ignores system HTTP proxy variables and refuses redirects. No additional
client token or authorization header is used: the unguessable, short-lived
run capability in the URL is the sole execution capability.

## Wire protocol v1

Ping request:

```json
{"version":1,"operation":"ping","alias":"orders"}
```

Query or exec request:

```json
{
  "version": 1,
  "operation": "query",
  "alias": "orders",
  "sql": "select id from orders where id = ?",
  "parameters": [
    {"ordinal": 1, "type": "int64", "value": "42"}
  ],
  "options": {"maxRows": 1000, "timeoutMs": 120000}
}
```

Supported parameter types are `null`, `bool`, `int64`, `float64`, `string`,
`bytes`, and `time`. Both `int64` and `float64` values are decimal strings;
`bytes` uses standard padded Base64 and `time` uses RFC3339Nano. This avoids
JSON number precision loss and rejects NaN or infinity.

Successful query response:

```json
{
  "version": 1,
  "ok": true,
  "columns": [
    {"name": "id", "databaseType": "BIGINT", "nullable": false}
  ],
  "rows": [
    [{"type": "int64", "value": "42"}]
  ],
  "affectedRows": "0",
  "lastInsertId": null,
  "truncated": false,
  "warnings": []
}
```

Successful exec response may omit `columns` and `rows` and return
`affectedRows` plus an optional string `lastInsertId`. Error response:

```json
{
  "version": 1,
  "ok": false,
  "error": {
    "code": "SQL_DENIED",
    "message": "write is not allowed",
    "retryable": false
  }
}
```

Response cell types additionally accept `text`, `json`, `decimal`, `date`, and
`datetime` as strings. Before encoding, the client rejects SQL larger than
256 KiB or more than 1024 parameters. It also rejects more than 1 MiB per
encoded request, 16 MiB per response, 512 columns, or 1000 rows. A response
marked `truncated` is an error so automation cannot mistake incomplete data
for a complete result.

## Security boundary

The managed build has two layers:

1. `no_base ipass` prevents direct database drivers from being linked.
2. The `managed` runtime policy rejects every driver except `ipass`, disables
   config/init/history/input/output files and external pagers, and exposes only
   a small safe set of backslash commands. Backtick expansion and shell, file,
   environment, copy, editor, chart, and transaction meta-commands are blocked.

This client-side policy is defense in depth. The Adapter/iPaaS Action remains
responsible for binding the run capability to the current user and saved API
key, enforcing read/write and SQL policy for the selected connection, setting
database timeouts, capping rows, and returning the typed envelope above. A SQL
parser in the CLI cannot safely enforce policy across every database dialect.
