# Run without elevation. These regression checks replace Windows mutations with fakes.
[CmdletBinding()]
param()
. (Join-Path $PSScriptRoot 'LanTrial.Common.ps1')
$testDirectory = Join-Path ([IO.Path]::GetTempPath()) ('cloudnest-lan-test-' + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $testDirectory | Out-Null
$statePath = Join-Path $testDirectory 'state.json'
$script:assertions = 0

function Assert-LanTest {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw $Message }
    $script:assertions++
}

function Get-NetAdapter {
    param([switch]$Physical)
    $script:adapters
}
function Get-NetIPConfiguration {
    param($InterfaceIndex)
    [pscustomobject]@{ IPv4DefaultGateway = '10.5.32.1' }
}
function Get-NetIPAddress {
    param($InterfaceIndex, $AddressFamily)
    [pscustomobject]@{ IPAddress = '10.5.33.9'; PrefixLength = 20
        AddressState = 'Preferred'; SkipAsSource = $false }
}
function Get-CimInstance { param($ClassName, $Filter) @() }
function Get-NetFirewallRule { param($Name, $ErrorAction) }
function Remove-NetFirewallRule { }
function Get-LanProxyTarget {
    param([string]$Address, [int]$Port)
    $script:proxies["$Address/$Port"]
}
function netsh {
    $address = @($args | Where-Object { $_ -like 'listenaddress=*' })[0].Split('=')[1]
    $port = @($args | Where-Object { $_ -like 'listenport=*' })[0].Split('=')[1]
    if ($script:failDeletion) { $global:LASTEXITCODE = 1; return }
    $script:proxies.Remove("$address/$port")
    $global:LASTEXITCODE = 0
}

try {
    # 1. Select an unambiguous physical adapter and calculate a non-/24 subnet.
    $script:adapters = @([pscustomobject]@{ Name = 'WLAN'; Status = 'Up'; ifIndex = 11 })
    $selected = Get-LanTrialAddress ''
    Assert-LanTest ($selected.Address -eq '10.5.33.9') 'Wrong interface address.'
    Assert-LanTest ($selected.Subnet -eq '10.5.32.0/20') 'Subnet mask was not applied.'
    $message = ''
    try { Get-LanTrialAddress 'missing' | Out-Null } catch { $message = $_.Exception.Message }
    Assert-LanTest ($message -like '*Use -InterfaceAlias*') 'Missing adapter must give actionable guidance.'
    $script:adapters += [pscustomobject]@{ Name = 'Ethernet'; Status = 'Up'; ifIndex = 12 }
    $message = ''
    try { Get-LanTrialAddress '' | Out-Null } catch { $message = $_.Exception.Message }
    Assert-LanTest ($message -like '*WLAN, Ethernet*') 'Ambiguous selection must list candidates.'
    Assert-LanTest ((Get-LanTrialAddress 'WLAN').InterfaceAlias -eq 'WLAN') 'Explicit adapter failed.'

    # 2. Clean journaled old-IP resources while retaining unrelated forwarding.
    $script:failDeletion = $false
    $script:proxies = @{ '192.0.2.8/18081' = '127.0.0.1/18081'
        '192.0.2.8/19000' = '127.0.0.1/19000'; '203.0.113.1/19000' = '127.0.0.1/19000' }
    $journal = @{ Version = 1; Address = '192.0.2.8'; StoragePort = 19000 }
    Save-LanTrialState $journal
    Clear-LanTrial
    Assert-LanTest ($script:proxies.Count -eq 1) 'Old rules were not removed.'
    Assert-LanTest ($script:proxies.ContainsKey('203.0.113.1/19000')) 'Unrelated rule was removed.'
    Assert-LanTest (-not (Test-Path $statePath)) 'Successful cleanup must remove its journal.'
    Clear-LanTrial
    Assert-LanTest ($script:proxies.Count -eq 1) 'Repeated stop changed unrelated state.'

    # 3. Retry a partial setup and preserve evidence when deletion fails.
    Save-LanTrialState $journal
    $script:proxies['192.0.2.8/19000'] = '127.0.0.1/19000'
    $script:failDeletion = $true
    $message = ''
    try { Clear-LanTrial } catch { $message = $_.Exception.Message }
    Assert-LanTest ($message -like '*Could not remove*') 'Failed deletion must be reported.'
    Assert-LanTest (Test-Path $statePath) 'Failed cleanup lost its journal.'
    $script:failDeletion = $false
    Clear-LanTrial
    Assert-LanTest (-not (Test-Path $statePath)) 'Retry did not complete cleanup.'

    # 4. Reject cleanup of a forwarding target changed by another operator.
    Save-LanTrialState $journal
    $script:proxies['192.0.2.8/19000'] = '127.0.0.1/19999'
    $message = ''
    try { Clear-LanTrial } catch { $message = $_.Exception.Message }
    Assert-LanTest ($message -like '*changed externally*') 'Modified target must be detected.'
    Assert-LanTest ($script:proxies['192.0.2.8/19000'] -eq '127.0.0.1/19999') 'Modified rule was removed.'
    Assert-LanTest (Test-Path $statePath) 'Conflict lost its journal.'
    Write-Output "Passed $script:assertions LAN lifecycle regression checks."
} finally {
    # Only the known temporary files and empty test directory are removed.
    Remove-Item -LiteralPath $statePath -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath "$statePath.tmp" -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $testDirectory
}
