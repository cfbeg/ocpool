param([switch]$Rebuild, [int]$Port = 18071)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$warpState = Join-Path $projectRoot '.state\warp'
$buildDir = Join-Path $warpState 'build'
$tokenPath = Join-Path $warpState 'control-token.txt'
$envPath = Join-Path $warpState 'relay.env'
$containerName = 'ocpool-warp-1'
$imageName = 'ocpool-warp:local'
if ($Port -lt 1024 -or $Port -gt 65535) { throw 'Port must be between 1024 and 65535' }
if ((docker info --format '{{.OSType}}') -ne 'linux') { throw 'Docker must be running the Linux engine' }
New-Item -ItemType Directory -Path $buildDir -Force | Out-Null
# Limit access to the local operator and SYSTEM before creating control secrets.
$operatorSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
icacls $warpState /inheritance:r /grant:r "*${operatorSid}:(OI)(CI)F" '*S-1-5-18:(OI)(CI)F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not protect WARP state directory' }
if (-not (Test-Path -LiteralPath $tokenPath)) {
    $random = New-Object byte[] 32
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($random) } finally { $rng.Dispose() }
    $secret = [BitConverter]::ToString($random).Replace('-', '').ToLowerInvariant()
    [IO.File]::WriteAllText($tokenPath, $secret, [Text.Encoding]::ASCII)
}
$secret = [IO.File]::ReadAllText($tokenPath).Trim()
if ($secret -notmatch '^[a-f0-9]{64}$') { throw 'Existing WARP control token is invalid' }
[IO.File]::WriteAllText($envPath, "OCPOOL_RELAY_TOKEN=$secret`n", [Text.Encoding]::ASCII)
$existingImage = docker image ls $imageName --format '{{.Repository}}:{{.Tag}}'
if ($Rebuild -or -not $existingImage) {
    $priorOS = $env:GOOS; $priorArch = $env:GOARCH; $priorCGO = $env:CGO_ENABLED
    try {
        $env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
        Push-Location $projectRoot
        try { go build -trimpath -o (Join-Path $buildDir 'ocpool-egress') ./cmd/ocpool-egress } finally { Pop-Location }
        if ($LASTEXITCODE -ne 0) { throw 'Building Linux relay failed' }
    } finally { $env:GOOS = $priorOS; $env:GOARCH = $priorArch; $env:CGO_ENABLED = $priorCGO }
    Copy-Item -LiteralPath (Join-Path $projectRoot 'ops\warp\Dockerfile') -Destination (Join-Path $buildDir 'Dockerfile')
    $entrypoint = [IO.File]::ReadAllText((Join-Path $projectRoot 'ops\warp\entrypoint.sh')).Replace("`r`n", "`n")
    [IO.File]::WriteAllText((Join-Path $buildDir 'entrypoint.sh'), $entrypoint, (New-Object Text.UTF8Encoding($false)))
    docker build --tag $imageName $buildDir
    if ($LASTEXITCODE -ne 0) { throw 'Building WARP Docker image failed' }
}
$existingContainer = docker ps -a --filter "name=^/${containerName}$" --format '{{.Names}}'
if ($existingContainer) {
    if ($Rebuild) {
        docker stop --timeout 45 $containerName | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Stopping dedicated WARP container failed' }
        docker rm $containerName | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Replacing dedicated WARP container failed' }
    } else {
        docker start $containerName | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Starting dedicated WARP container failed' }
    }
}
if (-not $existingContainer -or $Rebuild) {
    docker volume create ocpool-warp-state | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Creating WARP registration volume failed' }
    docker run --detach --name $containerName --restart unless-stopped --cap-add NET_ADMIN --device /dev/net/tun --sysctl net.ipv6.conf.all.disable_ipv6=0 --publish "127.0.0.1:${Port}:18071" --env-file $envPath --mount 'type=volume,source=ocpool-warp-state,target=/var/lib/cloudflare-warp' $imageName | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Starting WARP Docker container failed' }
}
$healthy = $null
for ($attempt=0; $attempt -lt 90; $attempt++) {
    $state = docker inspect --format '{{.State.Status}}' $containerName
    if ($state -eq 'exited' -or $state -eq 'dead') { throw 'Dedicated WARP container exited; inspect docker logs ocpool-warp-1' }
    try {
        $healthy = Invoke-RestMethod -Uri "http://127.0.0.1:$Port/health" -Headers @{'X-Relay-Auth'=$secret} -TimeoutSec 7
        if ($healthy.healthy) { break }
    } catch {}
    Start-Sleep -Seconds 1
}
if (-not $healthy -or -not $healthy.healthy) { throw 'Dedicated WARP did not become ready. Inspect docker logs ocpool-warp-1 and /tmp/warp-svc.log inside the container.' }
[PSCustomObject]@{container=$containerName; relay_url="http://127.0.0.1:$Port"; token_file=$tokenPath; healthy=$healthy.healthy; public_ip=$healthy.public_ip; ip_scope=$healthy.ip_scope; warp=$healthy.warp} | ConvertTo-Json
