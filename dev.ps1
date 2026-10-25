# Windows development: .\dev.cmd [all|fe|be|setup|admin]
[CmdletBinding()]
param([ValidateSet('all', 'fe', 'be', 'setup', 'admin')][string]$Mode = 'all')

$ErrorActionPreference = 'Stop'

function Import-DevEnvironment([string]$Path) {
    $allowed = @('DATABASE_URL', 'HTTP_ADDR', 'APP_ORIGIN', 'TARGET_CREDENTIAL_KEY', 'FE_HTTP_ADDR', 'FE_BACKEND_URL')
    $values = @{}
    foreach ($line in Get-Content -LiteralPath $Path -Encoding UTF8) {
        if ($line -match '^\s*(#|$)') { continue }
        if ($line -notmatch '^\s*([A-Z_]+)\s*=(.*)$' -or $allowed -notcontains $Matches[1]) {
            throw 'Format .env tidak valid. Gunakan KEY=value sesuai .env.example.'
        }
        $name, $value = $Matches[1], $Matches[2].Trim()
        if ($value.Length -ge 2 -and (($value.StartsWith('"') -and $value.EndsWith('"')) -or ($value.StartsWith("'") -and $value.EndsWith("'")))) {
            $value = $value.Substring(1, $value.Length - 2)
        }
        if ($values.ContainsKey($name)) { throw "Key .env duplikat: $name" }
        $values[$name] = $value
    }
    # Parse all entries before applying; never execute .env as PowerShell.
    foreach ($name in $values.Keys) { [Environment]::SetEnvironmentVariable($name, $values[$name], 'Process') }
}

function Invoke-DevGo([string]$Go, [string]$Directory, [string[]]$GoArgs) {
    Push-Location $Directory
    try {
        & $Go @GoArgs
        if ($LASTEXITCODE -ne 0) { throw "Go gagal ($($GoArgs[0])). Periksa output di atas." }
    } finally { Pop-Location }
}

function Ensure-DevAdmin([string]$Go, [string]$Directory) {
    $status = Invoke-DevGo $Go $Directory @('run', './cmd/bootstrap', '-check')
    if ($status -eq 'true') { Write-Host 'Admin aplikasi sudah tersedia.'; return }
    if ($status -ne 'false') { throw 'Status admin tidak dapat diperiksa; server tidak dijalankan.' }
    do {
        $username = (Read-Host 'Username admin pertama').Trim()
        $password = Read-Host 'Password admin baru (12-1024 byte UTF-8)' -AsSecureString
        $plain = (New-Object System.Net.NetworkCredential('', $password)).Password
        $length = [System.Text.Encoding]::UTF8.GetByteCount($plain)
        $nameLength = [System.Text.Encoding]::UTF8.GetByteCount($username)
        $valid = $nameLength -ge 1 -and $nameLength -le 100 -and $username -notmatch '[ \t\r\n\x00]' -and $length -ge 12 -and $length -le 1024
        if (-not $valid) { Write-Warning 'Username harus 1-100 byte tanpa spasi/NUL; password harus 12-1024 byte. Ulangi input.' }
    } until ($valid)
    $env:BOOTSTRAP_USERNAME = $username
    $env:BOOTSTRAP_PASSWORD = $plain
    try { Invoke-DevGo $Go $Directory @('run', './cmd/bootstrap') }
    finally { Remove-Item Env:\BOOTSTRAP_PASSWORD -ErrorAction SilentlyContinue; $plain = $null }
}

function Get-DevURL([string]$Address) {
    if ($Address.StartsWith(':')) { $Address = "localhost$Address" }
    return [uri]"http://$Address"
}

function Assert-DevPort([uri]$URL) {
    $listener = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Any, $URL.Port)
    try {
        $listener.Server.ExclusiveAddressUse = $true
        $listener.Start()
    } catch { throw "Port $($URL.Port) sedang digunakan. Hentikan server lama sebelum menjalankan entrypoint." }
    finally { $listener.Stop() }
}

function Start-DevService([string]$Name, [string]$Binary, [string]$Directory, [string]$LogDirectory) {
    $stdout = Join-Path $LogDirectory "$Name.stdout.log"
    $stderr = Join-Path $LogDirectory "$Name.stderr.log"
    $process = Start-Process -FilePath $Binary -WorkingDirectory $Directory -WindowStyle Hidden -PassThru -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    return @{ Name = $Name; Process = $process; ErrorLog = $stderr }
}

function Assert-DevRunning($Service) {
    if ($Service.Process.HasExited) {
        if (Test-Path -LiteralPath $Service.ErrorLog) { Get-Content -LiteralPath $Service.ErrorLog -Tail 10 | Out-Host }
        throw "$($Service.Name) berhenti. Periksa log di tmp/dev/."
    }
}

function Wait-DevReady($Service, [string]$URL) {
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        Assert-DevRunning $Service
        try {
            $response = Invoke-WebRequest -Uri $URL -UseBasicParsing -TimeoutSec 2
            if ($response.StatusCode -eq 200) { return }
        } catch { }
        Start-Sleep -Milliseconds 200
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$($Service.Name) belum siap setelah 30 detik. Periksa log di tmp/dev/."
}

function Invoke-DevMain {
    $root = $PSScriptRoot
    $configPath = Join-Path $root '.env'
    $names = @('DATABASE_URL', 'HTTP_ADDR', 'APP_ORIGIN', 'TARGET_CREDENTIAL_KEY', 'FE_HTTP_ADDR', 'FE_BACKEND_URL', 'GOTOOLCHAIN', 'BOOTSTRAP_USERNAME', 'BOOTSTRAP_PASSWORD')
    $previous = @{}
    foreach ($name in $names) { $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
    $services = @()
    try {
        $goCommand = Get-Command go -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
        $go = if ($goCommand) { $goCommand.Source } else { Join-Path $env:ProgramFiles 'Go/bin/go.exe' }
        if (-not (Test-Path -LiteralPath $go)) { throw 'Go sistem tidak ditemukan. Pasang Go atau tambahkan Go yang sudah terpasang ke PATH.' }
        $env:GOTOOLCHAIN = 'local'
        if ($Mode -eq 'setup' -or ($Mode -ne 'fe' -and -not (Test-Path -LiteralPath $configPath) -and -not $env:DATABASE_URL)) {
            & (Join-Path $root 'scripts/dev-setup.ps1') -Root $root
        }
        $env:HTTP_ADDR = ':8080'
        $env:APP_ORIGIN = 'http://localhost:3000'
        $env:FE_HTTP_ADDR = ':3000'
        $env:FE_BACKEND_URL = 'http://localhost:8080'
        if (Test-Path -LiteralPath $configPath) { Import-DevEnvironment $configPath }
        if ($Mode -ne 'fe') {
            if (-not $env:DATABASE_URL -or -not $env:TARGET_CREDENTIAL_KEY) { throw 'DATABASE_URL dan TARGET_CREDENTIAL_KEY wajib diisi pada .env.' }
            Invoke-DevGo $go (Join-Path $root 'BE') @('run', './cmd/migrate')
        }
        if ($Mode -ne 'fe') { Ensure-DevAdmin $go (Join-Path $root 'BE') }
        if ($Mode -in @('setup', 'admin')) { Write-Host 'Setup selesai. Jalankan .\dev.cmd'; return }

        $selected = if ($Mode -eq 'all') { @('be', 'fe') } else { @($Mode) }
        $logDirectory = Join-Path $root 'tmp/dev'
        New-Item -ItemType Directory -Path $logDirectory -Force | Out-Null
        foreach ($name in $selected) {
            $address = if ($name -eq 'be') { $env:HTTP_ADDR } else { $env:FE_HTTP_ADDR }
            Assert-DevPort (Get-DevURL $address)
            $binary = Join-Path $logDirectory "$name.exe"
            $directory = Join-Path $root $name.ToUpperInvariant()
            $entry = if ($name -eq 'be') { './cmd/api' } else { '.' }
            Invoke-DevGo $go $directory @('build', '-o', $binary, $entry)
        }
        foreach ($name in $selected) {
            $service = Start-DevService $name (Join-Path $logDirectory "$name.exe") (Join-Path $root $name.ToUpperInvariant()) $logDirectory
            $services += $service
            $address = if ($name -eq 'be') { $env:HTTP_ADDR } else { $env:FE_HTTP_ADDR }
            $path = if ($name -eq 'be') { '/health/ready' } else { '/login' }
            Wait-DevReady $service "$((Get-DevURL $address).GetLeftPart([UriPartial]::Authority))$path"
            Write-Host "$($name.ToUpperInvariant()) siap: $(Get-DevURL $address)"
        }
        if ($Mode -ne 'be') { Write-Host "Buka aplikasi di browser: $env:APP_ORIGIN/login (alamat harus sesuai APP_ORIGIN)." }
        Write-Host 'Log: tmp/dev/*.log. Ctrl+C untuk menghentikan server yang dijalankan entrypoint ini.'
        if ($Mode -eq 'fe') { Write-Host "FE memakai backend: $env:FE_BACKEND_URL (jalankan BE terpisah untuk login/data)." }
        while ($true) {
            foreach ($service in $services) { Assert-DevRunning $service }
            Start-Sleep -Milliseconds 500
        }
    } finally {
        foreach ($service in $services) {
            if (-not $service.Process.HasExited) { $service.Process.Kill(); $service.Process.WaitForExit() }
            $service.Process.Dispose()
        }
        foreach ($name in $names) { [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process') }
    }
}

if ($MyInvocation.InvocationName -ne '.') { Invoke-DevMain }
