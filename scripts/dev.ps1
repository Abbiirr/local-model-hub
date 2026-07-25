<#
.SYNOPSIS
  LocalHub dev helper: build, test, vet, and run the daemon.

.DESCRIPTION
  Convenience wrapper around the Go toolchain for the reference box (Windows/PowerShell).
  Run from the repo root.

.PARAMETER Command
  One of:
    vet        - go vet ./...
    build-web  - npm run build in web/ (produces web/dist, the //go:embed payload)
    build      - build-web + go build ./... (also produces bin/localhub.exe)
    test       - go test ./...
    check      - build-web + vet + build + test (the gate)
    run        - build (incl. web) then run the daemon (passes through extra args)

.NOTES
  web/dist is embedded into the daemon via //go:embed (web/embed.go). It MUST exist at
  `go build` time or the Go build fails. The 'build'/'check'/'run' commands run the
  build-web step first so the embed payload is always fresh. To build the Go side alone
  without rebuilding the UI, run `go build ./...` directly (uses the existing web/dist).
  Node 24.11.1 via nvm is expected on PATH for build-web (see wiring-p15.md).

.EXAMPLE
  ./scripts/dev.ps1 check
.EXAMPLE
  ./scripts/dev.ps1 run -config configs/config.yaml
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('vet', 'build-web', 'build', 'test', 'check', 'run')]
    [string]$Command = 'check',

    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$Rest
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
try {
    # buildWeb produces web/dist, the //go:embed payload the daemon serves. Skipped only by
    # the standalone 'vet'/'test' commands (which don't recompile the embed).
    function buildWeb {
        Write-Host '==> npm run build (web/dist embed payload)' -ForegroundColor Cyan
        Push-Location (Join-Path $repoRoot 'web')
        try {
            npm run build
            if ($LASTEXITCODE -ne 0) { throw "web build failed (exit $LASTEXITCODE)" }
        }
        finally { Pop-Location }
    }

    switch ($Command) {
        'vet' {
            go vet ./...
        }
        'build-web' {
            buildWeb
        }
        'build' {
            buildWeb
            New-Item -ItemType Directory -Force -Path 'bin' | Out-Null
            go build -o 'bin/localhub.exe' ./cmd/localhub
            go build ./...
        }
        'test' {
            go test ./...
        }
        'check' {
            buildWeb
            Write-Host '==> go vet ./...' -ForegroundColor Cyan
            go vet ./...
            Write-Host '==> go build ./...' -ForegroundColor Cyan
            go build ./...
            Write-Host '==> go test ./...' -ForegroundColor Cyan
            go test ./...
            Write-Host 'OK: web build + go vet + build + test clean' -ForegroundColor Green
        }
        'run' {
            buildWeb
            New-Item -ItemType Directory -Force -Path 'bin' | Out-Null
            go build -o 'bin/localhub.exe' ./cmd/localhub
            & './bin/localhub.exe' @Rest
        }
    }
}
finally {
    Pop-Location
}
