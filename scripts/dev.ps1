#Requires -Version 5.1
<#
.SYNOPSIS
  The Makefile targets, for PowerShell. `make` is not present by default on
  Windows and this project is developed there.

.EXAMPLE
  .\scripts\dev.ps1 test
  .\scripts\dev.ps1 validate
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('help','test','vet','fmt','fmt-check','build','run','validate',
                 'freshness','docker','up','down','console-dev','tidy')]
    [string]$Target = 'help'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

switch ($Target) {
    'help' {
        Write-Output @'
  test         go test ./...
  vet          go vet ./...
  fmt          gofmt -w cmd internal
  fmt-check    fail if anything is unformatted
  build        build both binaries
  run          run the gateway against config/
  validate     check config and report price-attestation ages
  freshness    fail 14 days before the attestations would stop the gateway
  docker       build the image
  up           docker compose up --build
  down         docker compose down -v
  console-dev  run the console against a local gateway
  tidy         go mod tidy

  -race is deliberately absent: it needs a C toolchain that a stock Windows
  box does not have. CI runs it on Linux.
'@
    }
    'test'        { go test ./... }
    'vet'         { go vet ./... }
    'fmt'         { gofmt -w cmd internal }
    'fmt-check'   {
        $bad = gofmt -l cmd internal
        if ($bad) { Write-Error "unformatted:`n$bad" }
    }
    'build'       { go build -o relay.exe ./cmd/relay; go build -o relay-eval.exe ./cmd/relay-eval }
    'run'         { go run ./cmd/relay }
    'validate'    { go run ./cmd/relay -validate }
    'freshness'   { go run ./cmd/relay -validate -max-price-age 1848h }
    'docker'      { docker build -t relay:dev . }
    'up'          { docker compose up --build }
    'down'        { docker compose down -v }
    'console-dev' { Set-Location (Join-Path $root 'web'); npm run dev }
    'tidy'        { go mod tidy }
}
