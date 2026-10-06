param([string]$Listen='127.0.0.1:10000')
$ErrorActionPreference='Stop'
$projectRoot=Split-Path -Parent $PSScriptRoot
$next=Join-Path $projectRoot 'bin\ocpool-next.exe'
$binary=Join-Path $projectRoot 'bin\ocpool.exe'
$pidFile=Join-Path $projectRoot '.state\server\server.pid'
if (-not (Test-Path -LiteralPath $next)) { throw 'Build bin/ocpool-next.exe before restarting' }
if (Test-Path -LiteralPath $pidFile) {
  $serverId=[int](Get-Content -LiteralPath $pidFile)
  $info=Get-CimInstance Win32_Process -Filter "ProcessId = $serverId"
  if ($info -and $info.ExecutablePath -ne $binary) { throw 'Server PID belongs to a different executable' }
  if ($info) { $handle=Get-Process -Id $serverId; Stop-Process -Id $serverId -Force; if (-not $handle.WaitForExit(10000)) { throw 'Server did not exit' } }
}
for ($attempt=0; $attempt -lt 5; $attempt++) {
  try { Copy-Item -LiteralPath $next -Destination $binary -Force; break }
  catch { if ($attempt -eq 4) { throw }; Start-Sleep -Milliseconds 200 }
}
& (Join-Path $PSScriptRoot 'start-server.ps1') -Listen $Listen
