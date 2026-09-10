[CmdletBinding()]
param(
    [string]$OutputPath = "bin/usql-gateway.exe"
)

$ErrorActionPreference = "Stop"
$repositoryRoot = Resolve-Path (Join-Path $PSScriptRoot "../..")
$goExecutable = "C:\Program Files\Go\bin\go.exe"

if (-not (Test-Path -LiteralPath $goExecutable -PathType Leaf)) {
    throw "Go executable was not found at the configured path."
}

$resolvedOutput = Join-Path $repositoryRoot $OutputPath
$outputDirectory = Split-Path -Parent $resolvedOutput
New-Item -ItemType Directory -Path $outputDirectory -Force | Out-Null

Push-Location $repositoryRoot
try {
    & $goExecutable build -trimpath -tags "no_base postgres mysql sqlserver moderncsqlite" -o $resolvedOutput ./cmd/usql-gateway
    if ($LASTEXITCODE -ne 0) {
        throw "usql-gateway build failed."
    }
}
finally {
    Pop-Location
}

Write-Output $resolvedOutput
