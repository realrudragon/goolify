<#
.SYNOPSIS
  One-command launcher for the Coolify Standalone stack (Astro + Vue 3.5 + Go 1.26 + Rust coolify-executor + PostgreSQL 16 + Redis 7).

.EXAMPLE
  .\coolify.ps1
  .\coolify.ps1 start
  .\coolify.ps1 status
  .\coolify.ps1 logs controlplane
  .\coolify.ps1 cli schedule:diagnostics
  .\coolify.ps1 stop
  .\coolify.ps1 down
#>
param(
  [Parameter(Position = 0)]
  [string]$Command = 'start',

  [Parameter(ValueFromRemainingArguments = $true)]
  [string[]]$Rest = @()
)

$ErrorActionPreference = 'Stop'
$RepoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$ComposeFile = Join-Path $RepoRoot 'compose.yml'

function Invoke-Compose {
  param([string[]]$ComposeArgs)
  & docker compose -f $ComposeFile @ComposeArgs
  if ($LASTEXITCODE -ne 0) {
    exit $LASTEXITCODE
  }
}

function Show-Usage {
  Write-Host @"
Coolify Standalone Launcher

Usage:
  .\coolify.ps1 [start]        Start stack in background (auto-bootstraps DB + root user)
  .\coolify.ps1 stop           Stop containers (data is kept)
  .\coolify.ps1 down|destroy   Stop and remove containers (volumes are kept)
  .\coolify.ps1 restart        Restart stack containers
  .\coolify.ps1 ps|status      Show service status
  .\coolify.ps1 logs [svc...]  Follow logs
  .\coolify.ps1 cli <args...>  Run /usr/local/bin/coolify-cli inside controlplane
"@
}

switch ($Command.ToLowerInvariant()) {
  { $_ -in @('--help', '-h', 'help') } {
    Show-Usage
    break
  }
  { $_ -in @('start', 'up') } {
    Invoke-Compose (@('up', '-d', '--build') + $Rest)
    $port = if ($env:APP_PORT) { $env:APP_PORT } else { '8000' }
    $appUrl = if ($env:APP_URL) { $env:APP_URL } else { "http://localhost:$port" }
    $email = if ($env:ROOT_USER_EMAIL) { $env:ROOT_USER_EMAIL } else { 'admin@coolify.local' }
    $pass = if ($env:ROOT_USER_PASSWORD) { $env:ROOT_USER_PASSWORD } else { 'CoolifyAdmin123!' }
    Write-Host ""
    Write-Host "Coolify Standalone is running!" -ForegroundColor Green
    Write-Host "  URL:      $appUrl"
    Write-Host "  Email:    $email"
    Write-Host "  Password: $pass"
    Write-Host ""
    Write-Host "Tip: Run '.\coolify.ps1 logs controlplane' to view logs or '.\coolify.ps1 stop' to stop."
    break
  }
  'stop' {
    Invoke-Compose (@('stop') + $Rest)
    break
  }
  { $_ -in @('down', 'destroy') } {
    Invoke-Compose (@('down', '--remove-orphans') + $Rest)
    break
  }
  'restart' {
    Invoke-Compose (@('restart') + $Rest)
    break
  }
  { $_ -in @('ps', 'status') } {
    Invoke-Compose (@('ps') + $Rest)
    break
  }
  'logs' {
    Invoke-Compose (@('logs', '-f', '--tail=100') + $Rest)
    break
  }
  'cli' {
    if ($Rest.Count -eq 0) {
      Invoke-Compose @('exec', 'controlplane', '/usr/local/bin/coolify-cli', '--help')
    } else {
      Invoke-Compose (@('exec', 'controlplane', '/usr/local/bin/coolify-cli') + $Rest)
    }
    break
  }
  default {
    Write-Error "Unknown command: $Command"
    Show-Usage
    exit 2
  }
}
