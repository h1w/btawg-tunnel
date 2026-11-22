param(
    [Parameter(Mandatory)][string] $Version,
    [Parameter(Mandatory)][string] $OutDir
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$OutDir = [IO.Path]::GetFullPath($OutDir)
$null = New-Item -ItemType Directory -Force $OutDir
$stage = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid().ToString())
$null = New-Item -ItemType Directory $stage
try {
    $source = Join-Path $stage 'wintun.zip'
    Invoke-WebRequest -Uri 'https://www.wintun.net/builds/wintun-0.14.1.zip' -OutFile $source -UseBasicParsing
    $expected = '07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51'
    if ((Get-FileHash -Algorithm SHA256 $source).Hash -ne $expected) { throw 'Wintun archive checksum mismatch' }
    Expand-Archive -LiteralPath $source -DestinationPath $stage
    $bundle = Join-Path $stage 'bundle'
    New-Item -ItemType Directory $bundle | Out-Null
    Copy-Item (Join-Path $stage 'wintun/bin/amd64/wintun.dll') $bundle
    Copy-Item (Join-Path $stage 'wintun/LICENSE.txt') (Join-Path $bundle 'Wintun-LICENSE.txt')
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    Push-Location $root
    try {
        go build -trimpath -ldflags "-s -w -X bittorrent-tunnel/btawg/internal/service.Version=$Version" -o (Join-Path $bundle 'btawg-client.exe') ./cmd/btawg-client
        if ($LASTEXITCODE -ne 0) { throw 'Windows client build failed' }
    } finally { Pop-Location }
    $versionOutput = & (Join-Path $bundle 'btawg-client.exe') --version
    if ($LASTEXITCODE -ne 0 -or $versionOutput -ne $Version) { throw "unexpected Windows client version: $versionOutput" }
    $archive = Join-Path $OutDir 'btawg-client-windows-amd64.zip'
    Compress-Archive -Path (Join-Path $bundle '*') -DestinationPath $archive -Force
    $hash = (Get-FileHash -Algorithm SHA256 $archive).Hash.ToLowerInvariant()
    "$hash  btawg-client-windows-amd64.zip" | Set-Content -LiteralPath (Join-Path $OutDir 'SHA256SUMS.windows') -Encoding ASCII
} finally {
    Remove-Item -LiteralPath $stage -Recurse -Force
}
