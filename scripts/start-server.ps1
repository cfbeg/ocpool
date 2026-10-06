param([string]$Listen = '127.0.0.1:10000')
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$serverBinary = Join-Path $projectRoot 'bin\ocpool.exe'
$serverState = Join-Path $projectRoot '.state\server'
if (-not (Test-Path -LiteralPath $serverBinary)) { throw 'Build the server first: go build -o bin/ocpool.exe ./cmd/ocpool' }
if ($Listen -notmatch '^[a-zA-Z0-9.:[\]-]+$') { throw 'Invalid listen address' }
New-Item -ItemType Directory -Path $serverState -Force | Out-Null
$serverProcess = Start-Process -FilePath $serverBinary -ArgumentList @('--listen', $Listen) -WorkingDirectory $projectRoot -WindowStyle Hidden -PassThru -RedirectStandardOutput (Join-Path $serverState 'stdout.log') -RedirectStandardError (Join-Path $serverState 'stderr.log')
$serverProcess.Id | Set-Content -LiteralPath (Join-Path $serverState 'server.pid') -Encoding ASCII
$probeAddress=$Listen -replace '^0\.0\.0\.0:', '127.0.0.1:' -replace '^\[::\]:', '[::1]:'
$ready=$false
for ($attempt=0; $attempt -lt 60; $attempt++) {
  $serverProcess.Refresh()
  if ($serverProcess.HasExited) { throw "Server exited during startup. See $serverState\stderr.log" }
  try { $health=Invoke-RestMethod -Uri "http://$probeAddress/health" -TimeoutSec 1; if ($health.ok) { $ready=$true; break } } catch {}
  Start-Sleep -Milliseconds 200
}
if (-not $ready) { throw 'Server did not become ready' }
Write-Output "OCPool process: $($serverProcess.Id)"
Write-Output "Login link: $(Join-Path $serverState 'access-url.txt')"
