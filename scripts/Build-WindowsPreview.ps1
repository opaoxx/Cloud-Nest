[CmdletBinding()]
param(
    [string]$Makensis = 'makensis.exe',
    [string]$AppBinary,
    [string]$OutputDirectory,
    [ValidateRange(1, 65535)]
    [int]$PreviewRevision = 1
)

$ErrorActionPreference = 'Stop'
$repository = Split-Path -Parent $PSScriptRoot
$nsisCompiler = (Get-Command -Name $Makensis -CommandType Application).Source
$version = "2.0.0-preview.$PreviewRevision"
$fileVersion = "2.0.0.$PreviewRevision"
$artifacts = if ($OutputDirectory) {
    $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($OutputDirectory)
} elseif ($PreviewRevision -eq 1) {
    Join-Path $repository 'output/desktop-step36'
} else {
    Join-Path $repository "output/desktop-preview.$PreviewRevision"
}
$buildFromSource = -not $AppBinary
if ($buildFromSource) {
    $AppBinary = Join-Path $artifacts 'CloudNest.exe'
} else {
    $appFile = Get-Item -LiteralPath $AppBinary
    if ($appFile.PSIsContainer) { throw '请输入 CloudNest 可执行文件的路径。' }
    $AppBinary = $appFile.FullName
}
$installer = Join-Path $artifacts "CloudNest-$version-windows-x64-setup.exe"
$installerScript = Join-Path $repository 'apps/desktop/installer/windows-preview.nsi'

Push-Location -LiteralPath $repository
try {
    # 1. 默认从源码构建。传入已验收的程序时，直接检查那一份文件。
    if ($buildFromSource) {
        $goTarget = @(go env GOOS GOARCH)
        if ($LASTEXITCODE -ne 0 -or $goTarget.Count -ne 2 -or $goTarget[0] -ne 'windows' -or $goTarget[1] -ne 'amd64') {
            throw '试用包需要 Go 的 windows/amd64 构建目标。'
        }
        New-Item -ItemType Directory -Path $artifacts -Force | Out-Null
        go build -mod=readonly -ldflags '-H=windowsgui' -o $AppBinary ./apps/desktop
        if ($LASTEXITCODE -ne 0) { throw 'CloudNest 构建失败。' }
    }
    $buildInfo = @(go version -m $AppBinary 2>$null)
    if ($LASTEXITCODE -ne 0 -or
        -not ($buildInfo -match '^\s+path\s+github\.com/opaoxx/cloud-nest/apps/desktop$') -or
        -not ($buildInfo -match '^\s+build\s+GOOS=windows$') -or
        -not ($buildInfo -match '^\s+build\s+GOARCH=amd64$')) {
        throw '安装包需要 CloudNest Windows x64 桌面程序。'
    }

    # 2. 把选定的程序和试用版本写入当前用户安装包。
    New-Item -ItemType Directory -Path $artifacts -Force | Out-Null
    $nsisArguments = @(
        '/WX', '/V3', '/INPUTCHARSET', 'UTF8',
        "/DAPP_BINARY=$AppBinary", "/DINSTALLER_PATH=$installer",
        "/DPRODUCT_VERSION=$version", "/DPRODUCT_FILE_VERSION=$fileVersion", $installerScript
    )
    & $nsisCompiler @nsisArguments
    if ($LASTEXITCODE -ne 0) { throw 'NSIS 安装包构建失败。' }
    Get-Item -LiteralPath $installer
}
finally {
    Pop-Location
}
