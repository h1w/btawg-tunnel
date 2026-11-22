$ErrorActionPreference = 'Stop'

$launch = {
    $ErrorActionPreference = 'Stop'
    $service = Get-CimInstance -ClassName Win32_Service -Filter "Name='btawg-client'"
    if (-not $service) {
        throw 'btawg-client is not installed. Open the client from its extracted ZIP and choose Install service first.'
    }

    # Use the currently registered bundle and omit the service arguments.
    if ($service.PathName -notmatch '^"([^"\r\n]+)"\s+run\s+-managed\s*$') {
        throw 'The btawg-client service does not have the expected launch command.'
    }
    $exe = [IO.Path]::GetFullPath($Matches[1])
    $bundle = [IO.Path]::GetDirectoryName($exe)
    $programFiles = if ($env:ProgramW6432) { $env:ProgramW6432 } else { [Environment]::GetFolderPath('ProgramFiles') }
    $installRoot = Join-Path $programFiles 'btawg'
    if ([IO.Path]::GetFileName($exe) -ne 'btawg-client.exe' -or
        [IO.Path]::GetFileName($bundle) -notlike 'bundle-?*' -or
        [IO.Path]::GetDirectoryName($bundle) -ne $installRoot -or
        -not (Test-Path -LiteralPath $exe -PathType Leaf)) {
        throw 'The service executable is missing or is not in an installed btawg bundle.'
    }

    Push-Location -LiteralPath $bundle
    try { & $exe } finally { Pop-Location }
}

$principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
if ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    & $launch
} else {
    # Elevate only the fixed command. Do not reopen a user-writable .ps1 as admin.
    $command = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($launch.ToString()))
    $powershell = Join-Path ([Environment]::SystemDirectory) 'WindowsPowerShell\v1.0\powershell.exe'
    Start-Process -FilePath $powershell -ArgumentList '-NoProfile', '-EncodedCommand', $command -Verb RunAs
}
