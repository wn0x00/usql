[CmdletBinding()]
param(
    [string] $TargetOS = "",
    [string] $TargetArch = "",
    [string] $OutputDirectory = "",

    [ValidatePattern("^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$")]
    [string] $Version = "0.0.0-ipass-managed",

    [string] $GoExecutable = "go",
    [switch] $SkipTests
)

$ErrorActionPreference = "Stop"
$originalLocation = Get-Location
$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot ".."))

if ([IO.Path]::IsPathRooted($GoExecutable)) {
    if (-not (Test-Path -LiteralPath $GoExecutable -PathType Leaf)) {
        throw "Go executable does not exist: $GoExecutable"
    }
    $go = $GoExecutable
} else {
    $goCommand = Get-Command $GoExecutable -ErrorAction Stop
    $go = $goCommand.Source
}

$hostOS = (& $go env GOHOSTOS).Trim()
$hostArch = (& $go env GOHOSTARCH).Trim()
if ($LASTEXITCODE -ne 0) {
    throw "Unable to inspect the Go toolchain"
}
if ([string]::IsNullOrWhiteSpace($TargetOS)) {
    $TargetOS = $hostOS
}
if ([string]::IsNullOrWhiteSpace($TargetArch)) {
    $TargetArch = $hostArch
}
if ($TargetOS -notin @("windows", "linux", "darwin")) {
    throw "TargetOS must be windows, linux or darwin"
}
if ($TargetArch -notin @("amd64", "arm64")) {
    throw "TargetArch must be amd64 or arm64"
}
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    $OutputDirectory = Join-Path $repoRoot "bin"
} elseif (-not [IO.Path]::IsPathRooted($OutputDirectory)) {
    $OutputDirectory = Join-Path $repoRoot $OutputDirectory
}

$binaryName = if ($TargetOS -eq "windows") { "usql.exe" } else { "usql" }
$outputPath = Join-Path $OutputDirectory $binaryName
$tags = "managed no_base ipass"
$testPackages = @(
    ".",
    "./drivers/ipass/...",
    "./internal/managedpolicy",
    "./metacmd",
    "./handler",
    "./stmt"
)
$ldflags = "-s -w -X github.com/xo/usql/text.CommandName=usql -X github.com/xo/usql/text.CommandVersion=$Version"

$savedEnvironment = @{
    CGO_ENABLED = [Environment]::GetEnvironmentVariable("CGO_ENABLED", "Process")
    GOOS = [Environment]::GetEnvironmentVariable("GOOS", "Process")
    GOARCH = [Environment]::GetEnvironmentVariable("GOARCH", "Process")
}

try {
    Set-Location -LiteralPath $repoRoot
    $env:CGO_ENABLED = "0"
    $env:GOOS = $hostOS
    $env:GOARCH = $hostArch

    if (-not $SkipTests) {
        & $go test -tags $tags @testPackages
        if ($LASTEXITCODE -ne 0) {
            throw "Managed iPaaS tests failed"
        }
    }

    New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
    $env:GOOS = $TargetOS
    $env:GOARCH = $TargetArch
    & $go build -tags $tags -trimpath -ldflags $ldflags -o $outputPath .
    if ($LASTEXITCODE -ne 0) {
        throw "Managed iPaaS build failed"
    }

    if ($TargetOS -eq $hostOS -and $TargetArch -eq $hostArch) {
        if ((& $outputPath --has-ipass-support) -ne "1") {
            throw "Built executable does not contain the iPaaS driver"
        }
        if ((& $outputPath --has-postgres-support) -ne "0") {
            throw "Built executable unexpectedly contains a direct PostgreSQL driver"
        }
    }

    $checksum = (Get-FileHash -LiteralPath $outputPath -Algorithm SHA256).Hash.ToLowerInvariant()
    $checksumPath = "$outputPath.sha256"
    $checksumLine = "$checksum  $binaryName`n"
    [IO.File]::WriteAllText($checksumPath, $checksumLine, [Text.UTF8Encoding]::new($false))

    Write-Host "Managed iPaaS executable: $outputPath"
    Write-Host "SHA256 manifest: $checksumPath"
} finally {
    Set-Location -LiteralPath $originalLocation
    foreach ($name in $savedEnvironment.Keys) {
        $value = $savedEnvironment[$name]
        if ($null -eq $value) {
            Remove-Item -LiteralPath "Env:$name" -ErrorAction SilentlyContinue
        } else {
            [Environment]::SetEnvironmentVariable($name, $value, "Process")
        }
    }
}
