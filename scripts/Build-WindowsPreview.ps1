param(
    [string]$Makensis = 'makensis.exe'
)

$ErrorActionPreference = 'Stop'
$repository = Split-Path -Parent $PSScriptRoot
$nsisCompiler = (Get-Command -Name $Makensis -CommandType Application).Source
$artifacts = Join-Path $repository 'output/desktop-step36'
$appBinary = Join-Path $artifacts 'CloudNest.exe'
$installer = Join-Path $artifacts 'CloudNest-2.0.0-preview.1-windows-x64-setup.exe'
$installerScript = Join-Path $repository 'apps/desktop/installer/windows-preview.nsi'

Push-Location -LiteralPath $repository
try {
    # 1. 核对当前 Go 配置，构建嵌入窗口资源的 Windows x64 程序。
    $goTarget = @(go env GOOS GOARCH)
    if ($LASTEXITCODE -ne 0 -or $goTarget.Count -ne 2 -or $goTarget[0] -ne 'windows' -or $goTarget[1] -ne 'amd64') {
        throw '试用包需要 Go 的 windows/amd64 构建目标。'
    }
    New-Item -ItemType Directory -Path $artifacts -Force | Out-Null
    go build -mod=readonly -ldflags '-H=windowsgui' -o $appBinary ./apps/desktop
    if ($LASTEXITCODE -ne 0) { throw 'CloudNest 构建失败。' }

    # 2. 用刚构建的程序生成当前用户安装包。
    & $nsisCompiler /WX /V3 /INPUTCHARSET UTF8 "/DAPP_BINARY=$appBinary" "/DINSTALLER_PATH=$installer" $installerScript
    if ($LASTEXITCODE -ne 0) { throw 'NSIS 安装包构建失败。' }
    Get-Item -LiteralPath $installer
}
finally {
    Pop-Location
}
