param(
    [Parameter(Mandatory)][string] $Bundle,
    [Parameter(Mandatory)][string] $Fixture,
    [Parameter(Mandatory)][ValidatePattern('^[0-9a-fA-F]{64}$')][string] $ExpectedSha256
)
$ErrorActionPreference = 'Stop'
if ((Get-FileHash -Algorithm SHA256 -LiteralPath $Bundle).Hash -ne $ExpectedSha256) { throw 'bundle SHA-256 mismatch' }
$f = Get-Content -Raw -LiteralPath $Fixture | ConvertFrom-Json
$os = Get-CimInstance Win32_OperatingSystem
if ($os.ProductType -ne 1 -or [int]$os.BuildNumber -lt 22000) { throw 'release acceptance requires Windows 11' }
$echoHost = ([uri]$f.EchoURL).Host
if ([uri]::CheckHostName($echoHost) -ne [System.UriHostNameType]::Dns) { throw 'SOCKS DNS proof requires a fixture hostname' }
if ((Get-Service -Name btawg-client -ErrorAction SilentlyContinue) -or (Test-Path (Join-Path $env:ProgramData 'btawg/client.json'))) {
    throw 'use a clean isolated Windows VM; this harness must not touch an existing client'
}
$root = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString())
New-Item -ItemType Directory $root | Out-Null
Expand-Archive -LiteralPath $Bundle -DestinationPath $root
$client = Join-Path $root 'btawg-client.exe'
Write-Output "Testing $($os.Caption) build $($os.BuildNumber), SHA-256 $ExpectedSha256"
$defaults = Get-NetRoute -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' | Sort-Object InterfaceIndex,NextHop | Select-Object InterfaceIndex,NextHop,RouteMetric | ConvertTo-Json -Compress
& $client install -import-file $f.ImportFile
if ($LASTEXITCODE -ne 0) { throw 'native service install failed' }
if ((Get-Service -Name btawg-client).Status -ne 'Running') { throw 'native service did not reach Running' }
$v = Get-NetIPAddress -AddressFamily IPv4 -IPAddress $f.VPNIP -ErrorAction Stop
if (!$v) { throw 'imported VPN IPv4 not configured' }
$after = Get-NetRoute -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' | Sort-Object InterfaceIndex,NextHop | Select-Object InterfaceIndex,NextHop,RouteMetric | ConvertTo-Json -Compress
if ($after -ne $defaults) { throw 'client changed the Windows default route' }

# The operator exercises the same dashboard, starts all paths and changes peer
# and route policies here. The Linux fixture must observe each path independently.
Write-Output 'In the dashboard enable all ten transports and all three balancers; check P2P and egress as prescribed by the fixture.'
& $client
if ($LASTEXITCODE -ne 0) { throw 'Windows dashboard failed' }
if ((Get-Service -Name btawg-client).Status -ne 'Running') { throw 'closing the dashboard stopped the service' }
$clientConfig = Get-Content -Raw (Join-Path $env:ProgramData 'btawg/client.json') | ConvertFrom-Json
$ports = 1080..1092
foreach ($port in $ports) {
    $curlSettings = "socks5-hostname = 127.0.0.1:$port`nproxy-user = $($clientConfig.credentials.username):$($clientConfig.credentials.password)`n"
    $body = $curlSettings | & curl.exe -q --config - --noproxy '' --silent --show-error --fail --max-time 10 $f.EchoURL
    if ($LASTEXITCODE -ne 0 -or ($body -join "`n") -ne $f.EchoBody) { throw "SOCKS port $port failed" }
    Write-Output "SOCKS TCP/DNS $port PASS"
}
$body = & curl.exe -q --proxy '' --noproxy '*' --silent --show-error --fail --max-time 10 $f.PeerEchoURL
if ($LASTEXITCODE -ne 0 -or ($body -join "`n") -ne $f.EchoBody) { throw 'plain VPN TCP to Linux peer failed' }
Write-Output 'Windows kernel VPN TCP to Linux peer PASS'
$after = Get-NetRoute -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' | Sort-Object InterfaceIndex,NextHop | Select-Object InterfaceIndex,NextHop,RouteMetric | ConvertTo-Json -Compress
if ($after -ne $defaults) { throw 'dashboard changes modified the Windows default route' }
$required = @('tui', 'service-autostart-reboot', 'socks-udp-1080-1092', 'egress-acl', 'dht-eight-carriers', 'balancer-path-loss-recovery', 'peer-direct-ten-modes', 'peer-override-denial', 'peer-relay', 'nat-mapping', 'update-rollback')
$observed = Get-Content -Raw -LiteralPath $f.ObservationFile | ConvertFrom-Json
if ($observed.sha256 -ne $ExpectedSha256 -or $observed.platform -ne 'windows-11') { throw 'parity observation does not belong to this ZIP and Windows 11' }
foreach ($scenario in $required) {
    $record = $observed.scenarios.PSObject.Properties[$scenario]
    if (!$record -or $record.Value.passed -ne $true -or [string]::IsNullOrWhiteSpace($record.Value.evidence)) { throw "missing passing evidence: $scenario" }
}
Write-Output "Windows parity accepted: windows-11 $ExpectedSha256"
