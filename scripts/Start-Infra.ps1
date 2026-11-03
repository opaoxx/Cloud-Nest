[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$composePath = Join-Path $PSScriptRoot 'docker-compose.yml'
$environmentPath = Join-Path $PSScriptRoot '.env'
$sourceArchive = Join-Path $PSScriptRoot 'minio/minio-source.tar.gz'
$sourceHash = '45521908307306e925c98d629e1c17d78c8b72b6ee242b1bfb1409f7d8ee5841'

if (-not (Test-Path -LiteralPath $environmentPath)) {
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot '.env.example') -Destination $environmentPath
}

Push-Location $projectRoot
try {
    docker compose -f $composePath config --quiet
    if ($LASTEXITCODE -ne 0) { throw 'Compose configuration is invalid.' }

    python (Join-Path $PSScriptRoot 'check_infra_ports.py')
    if ($LASTEXITCODE -ne 0) { throw 'Port or subnet preflight failed.' }

    if (-not (Test-Path -LiteralPath $sourceArchive)) {
        Invoke-WebRequest -Uri 'https://codeload.github.com/minio/minio/tar.gz/9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a' -OutFile $sourceArchive -TimeoutSec 120
    }
    if ((Get-FileHash -LiteralPath $sourceArchive -Algorithm SHA256).Hash.ToLowerInvariant() -ne $sourceHash) {
        throw 'MinIO source checksum differs from the pinned official release.'
    }

    docker compose -f $composePath build minio
    if ($LASTEXITCODE -ne 0) { throw 'MinIO source build failed.' }

    docker compose -f $composePath up -d --wait --wait-timeout 180
    if ($LASTEXITCODE -ne 0) { throw 'CloudNest dependencies are not healthy.' }

    docker compose -f $composePath ps
    if ($LASTEXITCODE -ne 0) { throw 'Unable to read CloudNest container status.' }
} finally {
    Pop-Location
}
