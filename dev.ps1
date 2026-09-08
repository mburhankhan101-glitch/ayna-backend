# Dev tasks for Windows / PowerShell.
#
# The Makefile assumes `make`, which Windows does not ship. This is the same
# set of tasks without that assumption.
#
#   .\dev.ps1 api       run the API (loads .env)
#   .\dev.ps1 worker    run the worker
#   .\dev.ps1 check     everything CI runs: fmt, vet, test
#   .\dev.ps1 test      go test -race ./...
#   .\dev.ps1 ready     check /readyz on a running API
#
# NOTE: this file is deliberately ASCII-only. Windows PowerShell 5.1 reads
# scripts as ANSI when there is no byte-order mark, so a stray em dash or
# curly quote corrupts string parsing and produces a baffling "string is
# missing the terminator" error many lines away from the real cause.

param(
    [Parameter(Position = 0)]
    [ValidateSet('api', 'worker', 'migrate', 'migrate-down', 'migrate-status', 'phone', 'check', 'test', 'ready', 'help')]
    [string]$Task = 'help'
)

$ErrorActionPreference = 'Stop'

function Import-DotEnv {
    # PowerShell has no equivalent of bash's `set -a`, so each line is parsed
    # and assigned individually. Values keep whatever they contain: a Postgres
    # password may hold almost any character, and over-eager cleaning corrupts
    # it silently. Only one matching pair of surrounding quotes is stripped.
    if (-not (Test-Path .env)) {
        Write-Host ""
        Write-Host "No .env file found." -ForegroundColor Yellow
        Write-Host "  Copy-Item .env.example .env"
        Write-Host "  then paste your Neon POOLED connection string into DATABASE_URL."
        Write-Host ""
        exit 1
    }

    $loaded = 0
    foreach ($line in Get-Content .env) {
        $trimmed = $line.Trim()
        if ($trimmed -eq '' -or $trimmed.StartsWith('#')) { continue }

        $idx = $trimmed.IndexOf('=')
        if ($idx -lt 1) { continue }

        $name = $trimmed.Substring(0, $idx).Trim()
        $value = $trimmed.Substring($idx + 1).Trim()

        if ($value.Length -ge 2) {
            $isDoubleQuoted = $value.StartsWith('"') -and $value.EndsWith('"')
            $isSingleQuoted = $value.StartsWith("'") -and $value.EndsWith("'")
            if ($isDoubleQuoted -or $isSingleQuoted) {
                $value = $value.Substring(1, $value.Length - 2)
            }
        }

        Set-Item -Path "Env:$name" -Value $value
        $loaded++
    }

    Write-Host "loaded $loaded variables from .env" -ForegroundColor DarkGray
}

function Invoke-Phone {
    # Maps the phone's own localhost:8080 to this machine's, over the USB
    # cable. Far less brittle than a LAN IP, which changes whenever the laptop
    # joins a different network -- and it does not require the phone and the
    # laptop to be on the same Wi-Fi at all.
    #
    # The tunnel does NOT survive unplugging the cable or rebooting the phone,
    # so this is a per-session step rather than something you set up once.
    #
    # adb ships with the Android SDK and is not on PATH by default.
    $adb = Join-Path $env:LOCALAPPDATA 'Android\Sdk\platform-tools\adb.exe'
    if (-not (Test-Path $adb)) {
        Write-Host "adb not found at $adb" -ForegroundColor Red
        Write-Host 'Install it with Android Studio, or edit this path to match your SDK.'
        exit 1
    }

    & $adb devices
    & $adb reverse tcp:8080 tcp:8080 | Out-Null
    Write-Host ''
    Write-Host 'active reverse tunnels:' -ForegroundColor Cyan
    & $adb reverse --list
}

function Invoke-Check {
    Write-Host "gofmt..." -ForegroundColor Cyan
    $unformatted = gofmt -l .
    if ($unformatted) {
        Write-Host "These files need gofmt:" -ForegroundColor Red
        $unformatted | ForEach-Object { Write-Host "  $_" }
        exit 1
    }

    Write-Host "go vet..." -ForegroundColor Cyan
    go vet ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host "go test -race..." -ForegroundColor Cyan
    go test -race ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host ""
    Write-Host "all checks passed" -ForegroundColor Green
}

function Invoke-Ready {
    # A 503 from /readyz is a real answer, not a transport failure, so the
    # body is printed rather than a PowerShell exception.
    try {
        $r = Invoke-WebRequest -Uri 'http://localhost:8080/readyz' -UseBasicParsing
        Write-Host $r.Content
    }
    catch {
        $resp = $_.Exception.Response
        if ($resp) {
            $reader = New-Object System.IO.StreamReader($resp.GetResponseStream())
            Write-Host ("HTTP " + [int]$resp.StatusCode) -ForegroundColor Yellow
            Write-Host $reader.ReadToEnd()
        }
        else {
            Write-Host "Could not reach localhost:8080. Is the API running?" -ForegroundColor Red
        }
    }
}

function Show-Help {
    Write-Host ""
    Write-Host "  .\dev.ps1 api       run the API (loads .env)"
    Write-Host "  .\dev.ps1 worker    run the worker"
    Write-Host "  .\dev.ps1 migrate   apply pending migrations"
    Write-Host "  .\dev.ps1 migrate-status   what is applied"
    Write-Host "  .\dev.ps1 phone     adb reverse, so the phone can reach localhost:8080"
    Write-Host "  .\dev.ps1 check     fmt + vet + test, same as CI"
    Write-Host "  .\dev.ps1 test      go test -race ./..."
    Write-Host "  .\dev.ps1 ready     check /readyz on a running API"
    Write-Host ""
}

switch ($Task) {
    'api' { Import-DotEnv; go run ./cmd/api }
    'worker' { Import-DotEnv; go run ./cmd/worker }
    'migrate' { Import-DotEnv; go run ./cmd/migrate up }
    'migrate-down' { Import-DotEnv; go run ./cmd/migrate down }
    'migrate-status' { Import-DotEnv; go run ./cmd/migrate status }
    'phone' { Invoke-Phone }
    'check' { Invoke-Check }
    'test' { go test -race ./... }
    'ready' { Invoke-Ready }
    default { Show-Help }
}
