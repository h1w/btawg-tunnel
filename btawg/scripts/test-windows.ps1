param([Parameter(Mandatory)][string] $Bundle)
$ErrorActionPreference = 'Stop'
$Bundle = Resolve-Path -LiteralPath $Bundle
$root = Split-Path -Parent $PSScriptRoot
$stage = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString())
$null = New-Item -ItemType Directory $stage
Push-Location $root
try {
    Expand-Archive -LiteralPath $Bundle -DestinationPath $stage
    $env:CGO_ENABLED = '0'
    $env:BTAWG_WINDOWS_NETWORK_TESTS = '1'
    $env:BTAWG_WINDOWS_BUNDLE_DRIVER = Join-Path $stage 'wintun.dll'
    $targets = @{
        tunsetup = @('./internal/tunsetup', '^Test(WindowsClientTUN|OwnedAdapter|Underlay)');
        client = @('./internal/client', '^Test(Windows|Overlay|Daemon|Native|PeerUnderlay)');
        carrier = @('./internal/carrier', '^TestTCPControlCarriesDuplexTraffic$');
        service = @('./internal/service', '^Test(ReleaseAssetMatchesTarget|WindowsManaged|WindowsBundle|UpdateLock|UpdateRecovery|UpdateActivation)');
        cli = @('./cmd/btawg-client', '^TestPeer');
        portmap = @('./internal/portmap', '^TestGateway');
    }
    foreach ($suite in $targets.Keys) {
        $test = Join-Path $stage "$suite.test.exe"
        go test -c -o $test $targets[$suite][0]
        if ($LASTEXITCODE -ne 0) { throw "$suite test build failed" }
        & $test -test.v -test.timeout=120s "-test.run=$($targets[$suite][1])"
        if ($LASTEXITCODE -ne 0) { throw "$suite Windows smoke failed" }
    }
} finally {
    Pop-Location
    Remove-Item -LiteralPath $stage -Recurse -Force
}
