# Run: powershell -NoProfile -ExecutionPolicy Bypass -File scripts/dev.test.ps1
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
. (Join-Path $root 'dev.ps1')
$testDirectory = Join-Path $root ('tmp/dev-test-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $testDirectory -Force | Out-Null
$previousAddress = $env:HTTP_ADDR
$previousOrigin = $env:APP_ORIGIN
$previousDatabase = $env:DATABASE_URL
$listener = $null
try {
    foreach ($path in @('dev.ps1', 'scripts/dev-setup.ps1', 'scripts/dev.test.ps1')) {
        $tokens, $parseErrors = $null, $null
        [System.Management.Automation.Language.Parser]::ParseFile((Join-Path $root $path), [ref]$tokens, [ref]$parseErrors) | Out-Null
        if ($parseErrors) { throw "PowerShell syntax error: $path" }
    }
    Write-Host 'PASS: script syntax'

    $envFile = Join-Path $testDirectory '.env'
    @'
# Quotes are supported; values are literal, never evaluated.
HTTP_ADDR="127.0.0.1:19371"
APP_ORIGIN='http://127.0.0.1:3000'
DATABASE_URL=$(throw 'must not execute')
'@ | Set-Content -LiteralPath $envFile -Encoding ascii
    Import-DevEnvironment $envFile
    if ($env:HTTP_ADDR -ne '127.0.0.1:19371' -or $env:APP_ORIGIN -ne 'http://127.0.0.1:3000' -or $env:DATABASE_URL -ne '$(throw ''must not execute'')') { throw 'Incorrect literal .env loading' }
    Write-Host 'PASS: .env quotes and literal secret values'

    foreach ($invalid in @("HTTP_ADDR=changed`nPATH=untrusted", "HTTP_ADDR=changed`nHTTP_ADDR=duplicate", 'not an assignment')) {
        $invalid | Set-Content -LiteralPath $envFile -Encoding ascii
        $rejected = $false
        try { Import-DevEnvironment $envFile } catch { $rejected = $true }
        if (-not $rejected -or $env:HTTP_ADDR -ne '127.0.0.1:19371') { throw 'Invalid .env accepted or partially applied' }
    }
    Write-Host 'PASS: invalid/duplicate/unsupported configuration rejected atomically'

    $listener = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Any, 0)
    $listener.Server.ExclusiveAddressUse = $true
    $listener.Start()
    $url = [uri]"http://127.0.0.1:$($listener.LocalEndpoint.Port)"
    $rejected = $false
    try { Assert-DevPort $url } catch { $rejected = $true }
    if (-not $rejected) { throw 'Occupied port was accepted' }
    $listener.Stop()
    Assert-DevPort $url
    if ((Get-DevURL ':8080').Host -ne '127.0.0.1') { throw 'Wildcard HTTP address failed' }
    Write-Host 'PASS: occupied port rejected; free port and wildcard address accepted'

    $rejected = $false
    try { & (Join-Path $root 'scripts/dev-setup.ps1') -Root $testDirectory } catch { $rejected = $_.Exception.Message -like '*.env sudah ada*' }
    if (-not $rejected) { throw 'Setup must refuse an existing .env before accessing PostgreSQL' }
    Write-Host 'PASS: setup preserves existing configuration and avoids database access'
} finally {
    if ($listener) { $listener.Stop() }
    $env:HTTP_ADDR = $previousAddress
    $env:APP_ORIGIN = $previousOrigin
    $env:DATABASE_URL = $previousDatabase
}
