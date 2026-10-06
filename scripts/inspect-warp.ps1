param([switch]$Rotate, [switch]$RenewRegistration, [ValidateRange(1,2)][int]$MaxAttempts = 1, [int]$Port = 18071)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$tokenPath = Join-Path $projectRoot '.state\warp\control-token.txt'
if (-not (Test-Path -LiteralPath $tokenPath)) { throw 'Run scripts/setup-warp.ps1 first' }
$secret = [IO.File]::ReadAllText($tokenPath).Trim()
$route = if ($Rotate -or $RenewRegistration) { '/rotate' } else { '/health' }
$method = if ($Rotate -or $RenewRegistration) { 'POST' } else { 'GET' }
$options = @{Uri="http://127.0.0.1:$Port$route"; Method=$method; Headers=@{'X-Relay-Auth'=$secret}; TimeoutSec=130}
if ($RenewRegistration) { $options.Body = @{renew_registration=$true; max_attempts=$MaxAttempts} | ConvertTo-Json -Compress; $options.ContentType='application/json' }
Invoke-RestMethod @options | ConvertTo-Json -Depth 5
