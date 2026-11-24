# Shared lifecycle operations for this checkout's Windows LAN trial.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$runtimeDirectory = Join-Path $projectRoot 'output/lan-trial'
$statePath = Join-Path $runtimeDirectory 'state.json'
$serverPath = Join-Path $runtimeDirectory 'cloudnest-lan-server.exe'
$hasher = [Security.Cryptography.SHA256]::Create()
try {
    $checkoutHash = [BitConverter]::ToString($hasher.ComputeHash(
        [Text.Encoding]::UTF8.GetBytes($projectRoot.ToLowerInvariant()))).Replace('-', '').Substring(0, 16)
} finally { $hasher.Dispose() }
$firewallName = "CloudNest-LanTrial-$checkoutHash"

function Enter-LanTrialLock {
    # 1. Require elevation before changing system-wide networking.
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Run this script in an administrator PowerShell window.'
    }
    # 2. Serialize start and stop across Windows sessions for this checkout.
    $mutex = [Threading.Mutex]::new($false, "Global\CloudNest-LanTrial-$checkoutHash")
    try {
        try { $acquired = $mutex.WaitOne(0) }
        catch [Threading.AbandonedMutexException] { $acquired = $true }
        if (-not $acquired) { throw 'Another CloudNest LAN start/stop operation is running.' }
        return $mutex
    } catch { $mutex.Dispose(); throw }
}

function Get-LanTrialAddress {
    param([string]$InterfaceAlias)
    # 1. Prefer connected physical adapters, excluding VPN and Docker adapters.
    $adapters = @(Get-NetAdapter -Physical | Where-Object Status -eq 'Up')
    if ($InterfaceAlias) { $adapters = @($adapters | Where-Object Name -eq $InterfaceAlias) }
    $candidates = @(foreach ($adapter in $adapters) {
        $configuration = Get-NetIPConfiguration -InterfaceIndex $adapter.ifIndex
        if (-not $configuration.IPv4DefaultGateway) { continue }
        foreach ($address in @(Get-NetIPAddress -InterfaceIndex $adapter.ifIndex -AddressFamily IPv4)) {
            if ($address.AddressState -ne 'Preferred' -or $address.SkipAsSource -or
                $address.IPAddress -match '^(127\.|169\.254\.)') { continue }
            # 2. Calculate the actual subnet, including non-/24 networks.
            $bytes = [Net.IPAddress]::Parse($address.IPAddress).GetAddressBytes()
            $network = for ($index = 0; $index -lt 4; $index++) {
                $bits = [Math]::Min(8, [Math]::Max(0, $address.PrefixLength - 8 * $index))
                $mask = 256 - [Math]::Pow(2, 8 - $bits)
                [int]$bytes[$index] -band [int]$mask
            }
            [pscustomobject]@{ InterfaceAlias = $adapter.Name; Address = $address.IPAddress
                Subnet = ($network -join '.') + '/' + $address.PrefixLength }
        }
    })
    # 3. Reject ambiguity before opening any network ports.
    if ($candidates.Count -ne 1) {
        $candidateNames = @($candidates | ForEach-Object InterfaceAlias) -join ', '
        throw "Expected one physical IPv4 interface with a gateway. Candidates: $candidateNames. Use -InterfaceAlias."
    }
    return $candidates[0]
}

function Get-LanProxyTarget {
    param([string]$Address, [int]$Port)
    $registryPath = 'HKLM:\SYSTEM\CurrentControlSet\Services\PortProxy\v4tov4\tcp'
    if (Test-Path -LiteralPath $registryPath) {
        $key = Get-Item -LiteralPath $registryPath
        try { return $key.GetValue("$Address/$Port", $null) }
        finally { $key.Close() }
    }
}

function Save-LanTrialState {
    param($State)
    # Publish the resource journal before creating persistent rules.
    $temporaryPath = "$statePath.tmp"
    $State | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $temporaryPath -Encoding UTF8
    Move-Item -LiteralPath $temporaryPath -Destination $statePath -Force
}

function Clear-LanTrial {
    # 1. Stop only the dedicated executable belonging to this checkout.
    $processes = @(Get-CimInstance Win32_Process -Filter "Name='cloudnest-lan-server.exe'")
    foreach ($process in $processes) {
        if ($process.ExecutablePath -and [IO.Path]::GetFullPath($process.ExecutablePath) -eq $serverPath) {
            $running = Get-Process -Id $process.ProcessId -ErrorAction SilentlyContinue
            if ($running -and $running.Path -eq $serverPath) { $running.Kill(); $running.WaitForExit() }
        }
    }
    # 2. Remove the checkout-specific firewall rule, including interrupted setup.
    Get-NetFirewallRule -Name $firewallName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
    if (-not (Test-Path -LiteralPath $statePath)) { return }
    $state = Get-Content -LiteralPath $statePath -Raw | ConvertFrom-Json
    $parsedAddress = $null
    if ($state.Version -ne 1 -or -not [Net.IPAddress]::TryParse($state.Address, [ref]$parsedAddress) -or
        $parsedAddress.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork -or
        $state.StoragePort -lt 1 -or $state.StoragePort -gt 65535) {
        throw "Invalid LAN journal. Inspect $statePath before retrying."
    }
    # 3. Delete only journaled forwarding rules whose target still matches.
    foreach ($port in @(18081, [int]$state.StoragePort)) {
        $target = Get-LanProxyTarget $state.Address $port
        if ($null -eq $target) { continue }
        if ($target -ne "127.0.0.1/$port") {
            throw "Forwarding rule $($state.Address):$port was changed externally. Journal retained."
        }
        & netsh interface portproxy delete v4tov4 "listenaddress=$($state.Address)" "listenport=$port" | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Could not remove forwarding rule on port $port." }
    }
    Remove-Item -LiteralPath $statePath
}

function Test-LanHttp {
    param([string]$Address)
    # Bypass host proxy settings for local health probes.
    Add-Type -AssemblyName System.Net.Http
    $handler = [Net.Http.HttpClientHandler]::new()
    $handler.UseProxy = $false
    $client = [Net.Http.HttpClient]::new($handler)
    $client.Timeout = [TimeSpan]::FromSeconds(3)
    try {
        $response = $client.GetAsync($Address).GetAwaiter().GetResult()
        try { return $response.IsSuccessStatusCode }
        finally { $response.Dispose() }
    } catch { return $false }
    finally { $client.Dispose() }
}
