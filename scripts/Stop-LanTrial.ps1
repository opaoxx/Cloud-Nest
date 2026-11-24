[CmdletBinding()]
param()

. (Join-Path $PSScriptRoot 'LanTrial.Common.ps1')
# 1. Serialize cleanup with startup, including repeated stop requests.
$mutex = Enter-LanTrialLock
try {
    # 2. Remove this checkout's API process and journaled network resources.
    Clear-LanTrial
    Write-Output 'CloudNest LAN trial stopped. Its forwarding and firewall rules have been removed.'
} finally {
    # 3. Always release the lifecycle lock for the next request.
    $mutex.ReleaseMutex()
    $mutex.Dispose()
}
