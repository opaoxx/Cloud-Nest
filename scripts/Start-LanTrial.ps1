[CmdletBinding()]
param([string]$InterfaceAlias)

. (Join-Path $PSScriptRoot 'LanTrial.Common.ps1')
# 1. Lock this checkout and recover resources from its previous run.
$mutex = Enter-LanTrialLock
$environmentBefore = @{}
$ownsNewRun = $false
Push-Location $projectRoot
try {
    Clear-LanTrial
    $network = Get-LanTrialAddress $InterfaceAlias
    if (Get-NetTCPConnection -State Listen -LocalPort 18081 -ErrorAction SilentlyContinue) {
        throw 'Port 18081 is occupied. Stop the existing API in its original terminal first.'
    }
    # 2. Read the existing Compose configuration without printing credentials.
    $composePath = Join-Path $PSScriptRoot 'docker-compose.yml'
    $configurationText = & docker compose -f $composePath config --format json
    if ($LASTEXITCODE -ne 0) { throw 'Cannot read Compose configuration. Prepare scripts/.env first.' }
    $compose = $configurationText | ConvertFrom-Json
    $minioBinding = @($compose.services.minio.ports | Where-Object target -eq 9000)
    $mysqlBinding = @($compose.services.mysql.ports | Where-Object target -eq 3306)
    if ($minioBinding.Count -ne 1 -or $mysqlBinding.Count -ne 1 -or
        $minioBinding[0].host_ip -ne '127.0.0.1' -or $mysqlBinding[0].host_ip -ne '127.0.0.1') {
        throw 'LAN trial expects one loopback binding each for Compose MySQL and MinIO.'
    }
    $storagePort = [int]$minioBinding[0].published
    if ($storagePort -eq 18081) { throw 'MinIO and API ports must differ.' }
    foreach ($port in @(18081, $storagePort)) {
        if ($null -ne (Get-LanProxyTarget $network.Address $port) -or
            $null -ne (Get-LanProxyTarget '0.0.0.0' $port)) {
            throw "An unmanaged portproxy rule already uses port $port."
        }
        $listeners = @(Get-NetTCPConnection -State Listen -LocalPort $port -ErrorAction SilentlyContinue)
        if (@($listeners | Where-Object LocalAddress -ne '127.0.0.1').Count -gt 0) {
            throw "An existing non-loopback listener uses port $port."
        }
    }
    # Windows PowerShell 5.1 can wrap redirected native stderr as errors.
    # Docker progress belongs on stderr; decide success using its exit code.
    try {
        $ErrorActionPreference = 'Continue'
        & docker compose -f $composePath up -d --no-build --wait --wait-timeout 120
        $composeExitCode = $LASTEXITCODE
    } finally { $ErrorActionPreference = 'Stop' }
    if ($composeExitCode -ne 0) { throw 'Infrastructure failed to start. See scripts/Start-Infra.ps1 for first-time setup.' }
    if (-not (Test-LanHttp "http://127.0.0.1:$storagePort/minio/health/ready")) {
        throw 'MinIO is not ready.'
    }
    New-Item -ItemType Directory -Path $runtimeDirectory -Force | Out-Null
    & go build -o $serverPath ./cmd/server
    if ($LASTEXITCODE -ne 0) { throw 'Backend build failed.' }
    if ((Get-Service iphlpsvc).Status -ne 'Running') {
        throw 'Windows IP Helper service must be running for portproxy.'
    }
    # 3. Journal before mutation so failure or a later start can clean up.
    Save-LanTrialState ([ordered]@{ Version = 1; Address = $network.Address
        StoragePort = $storagePort; InterfaceAlias = $network.InterfaceAlias })
    $ownsNewRun = $true
    foreach ($port in @(18081, $storagePort)) {
        & netsh interface portproxy add v4tov4 "listenaddress=$($network.Address)" "listenport=$port" `
            connectaddress=127.0.0.1 "connectport=$port" | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Could not create forwarding rule on port $port." }
    }
    New-NetFirewallRule -Name $firewallName -DisplayName 'CloudNest LAN trial (managed)' `
        -Direction Inbound -Action Allow -Profile Any -InterfaceAlias $network.InterfaceAlias `
        -LocalAddress $network.Address -RemoteAddress $network.Subnet -Protocol TCP `
        -LocalPort 18081,$storagePort | Out-Null
    # 4. Give only the child API this run's addresses and Compose credentials.
    $environment = @{
        CLOUDNEST_HTTP_ADDR = '127.0.0.1:18081'
        CLOUDNEST_DB_ADDRESS = "127.0.0.1:$($mysqlBinding[0].published)"
        CLOUDNEST_DB_NAME = 'cloudnest'
        CLOUDNEST_DB_USER = 'root'
        CLOUDNEST_DB_PASSWORD = [string]$compose.services.mysql.environment.MYSQL_ROOT_PASSWORD
        CLOUDNEST_S3_ENDPOINT = "http://127.0.0.1:$storagePort"
        CLOUDNEST_S3_PUBLIC_ENDPOINT = "http://$($network.Address):$storagePort"
        CLOUDNEST_S3_BUCKET = 'cloudnest'
        CLOUDNEST_S3_ACCESS_KEY = [string]$compose.services.minio.environment.MINIO_ROOT_USER
        CLOUDNEST_S3_SECRET_KEY = [string]$compose.services.minio.environment.MINIO_ROOT_PASSWORD
        CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT = [string]$compose.name
        CLOUDNEST_TEMP_DIR = (Join-Path $runtimeDirectory 'uploads-temp')
    }
    foreach ($name in $environment.Keys) {
        $environmentBefore[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        [Environment]::SetEnvironmentVariable($name, $environment[$name], 'Process')
    }
    $server = Start-Process -FilePath $serverPath -WorkingDirectory $projectRoot -WindowStyle Hidden `
        -RedirectStandardOutput (Join-Path $runtimeDirectory 'server.stdout.log') `
        -RedirectStandardError (Join-Path $runtimeDirectory 'server.stderr.log') -PassThru
    # 5. Verify both LAN paths before reporting success; failure rolls back.
    $deadline = [DateTime]::UtcNow.AddSeconds(45)
    do {
        if ($server.HasExited) { throw "Backend exited. See $runtimeDirectory/server.stderr.log" }
        $ready = (Test-LanHttp "http://$($network.Address):18081/health/ready") -and
            (Test-LanHttp "http://$($network.Address):$storagePort/minio/health/ready")
        if ($ready) { break }
        Start-Sleep -Milliseconds 500
    } while ([DateTime]::UtcNow -lt $deadline)
    if (-not $ready) { throw "LAN readiness timed out. See logs in $runtimeDirectory" }
    Write-Output "Interface: $($network.InterfaceAlias) / $($network.Subnet)"
    Write-Output "Phone/tablet service address: http://$($network.Address):18081"
    Write-Output 'Windows service address: http://127.0.0.1:18081'
    Write-Output "File endpoint: http://$($network.Address):$storagePort"
    Write-Output "Backend PID: $($server.Id). Logs: $runtimeDirectory"
    Write-Output 'Stop with scripts/Stop-LanTrial.ps1. Run Start again after changing networks.'
} catch {
    $startupError = $_
    if ($ownsNewRun) {
        try { Clear-LanTrial }
        catch { Write-Warning "Cleanup incomplete. Run Stop-LanTrial.ps1 again. $($_.Exception.Message)" }
    }
    throw $startupError
} finally {
    foreach ($name in $environmentBefore.Keys) {
        [Environment]::SetEnvironmentVariable($name, $environmentBefore[$name], 'Process')
    }
    Pop-Location
    $mutex.ReleaseMutex()
    $mutex.Dispose()
}
