#Requires -Version 5.1
<#
.SYNOPSIS
    Cross-compiles YakAIGateway into dist/:
      - windows amd64 (.exe)
      - linux   amd64
      - darwin  arm64 (Apple Silicon)

.DESCRIPTION
    Pure-Go stdlib project, so CGO is disabled for all targets. Debug info is
    stripped (-s -w) and paths are trimmed (-trimpath) for reproducible builds.

.PARAMETER SkipTests
    Skip the go vet / go test gate before building.

.EXAMPLE
    ./build.ps1

.EXAMPLE
    ./build.ps1 -SkipTests
#>
[CmdletBinding()]
param(
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'go toolchain not found in PATH'
}

Push-Location $PSScriptRoot
try {
    $dist = Join-Path $PSScriptRoot 'dist'
    if (Test-Path $dist) {
        Remove-Item $dist -Recurse -Force
    }
    New-Item -ItemType Directory -Path $dist | Out-Null

    if (-not $SkipTests) {
        Write-Host '==> go vet ./...'
        go vet ./...
        if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }

        Write-Host '==> go test ./...'
        go test ./...
        if ($LASTEXITCODE -ne 0) { throw 'go test failed' }
    }

    $targets = @(
        @{ GOOS = 'windows'; GOARCH = 'amd64'; Name = 'yakaigateway_windows_amd64.exe' },
        @{ GOOS = 'linux';   GOARCH = 'amd64'; Name = 'yakaigateway_linux_amd64' },
        @{ GOOS = 'darwin';  GOARCH = 'arm64'; Name = 'yakaigateway_darwin_arm64' }
    )

    $env:CGO_ENABLED = '0'
    foreach ($target in $targets) {
        $env:GOOS = $target.GOOS
        $env:GOARCH = $target.GOARCH
        $out = Join-Path $dist $target.Name
        Write-Host ("==> {0}/{1} -> dist/{2}" -f $target.GOOS, $target.GOARCH, $target.Name)
        go build -trimpath -ldflags '-s -w' -o $out .
        if ($LASTEXITCODE -ne 0) {
            throw ("go build failed for {0}/{1}" -f $target.GOOS, $target.GOARCH)
        }
    }

    Write-Host ''
    Get-ChildItem $dist |
        Sort-Object Name |
        Format-Table Name, @{ Name = 'SizeMB'; Expression = { [math]::Round($_.Length / 1MB, 2) } } -AutoSize
}
finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
    Pop-Location
}
