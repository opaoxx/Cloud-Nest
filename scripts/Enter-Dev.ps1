[CmdletBinding()]
param(
    [string]$AndroidSdkPath = 'F:\Android\Sdk'
)

$ErrorActionPreference = 'Stop'
if (-not (Test-Path -LiteralPath $AndroidSdkPath -PathType Container)) {
    throw "Android SDK directory does not exist: $AndroidSdkPath"
}

$env:ANDROID_HOME = $AndroidSdkPath
$env:ANDROID_SDK_ROOT = $AndroidSdkPath
$goWorkspace = (go env GOPATH).Trim().Split([IO.Path]::PathSeparator)[0]
if ($LASTEXITCODE -ne 0) { throw 'Unable to locate the Go workspace.' }

$devPaths = @(
    (Join-Path $AndroidSdkPath 'platform-tools'),
    (Join-Path $AndroidSdkPath 'cmdline-tools/latest/bin'),
    (Join-Path $goWorkspace 'bin')
)
foreach ($devPath in $devPaths) {
    if ((Test-Path -LiteralPath $devPath) -and $env:PATH.Split([IO.Path]::PathSeparator) -notcontains $devPath) {
        $env:PATH = $devPath + [IO.Path]::PathSeparator + $env:PATH
    }
}
if ([string]::IsNullOrWhiteSpace($env:CLOUDNEST_HTTP_ADDR)) {
    $env:CLOUDNEST_HTTP_ADDR = '127.0.0.1:18081'
}

Write-Output "Android SDK: $AndroidSdkPath"
Write-Output "CloudNest HTTP address: $env:CLOUDNEST_HTTP_ADDR"
Write-Output 'Development paths are loaded for this PowerShell process.'
