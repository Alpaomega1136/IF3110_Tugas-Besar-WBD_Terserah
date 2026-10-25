param(
    [Parameter(Mandatory = $true)][string]$Root,
    [ValidateRange(1, 65535)][int]$Port = 5432,
    [string]$AdminUser = 'postgres'
)
$ErrorActionPreference = 'Stop'

$configPath = Join-Path $Root '.env'
if (Test-Path -LiteralPath $configPath) {
    throw '.env sudah ada; setup tidak menimpa credential/database. Gunakan .\dev.cmd atau .\dev.cmd admin jika bootstrap belum selesai.'
}
$psqlCommand = Get-Command psql -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
$psql = if ($psqlCommand) { $psqlCommand.Source } else { Join-Path $env:ProgramFiles 'PostgreSQL/18/bin/psql.exe' }
if (-not (Test-Path -LiteralPath $psql)) { throw 'psql tidak ditemukan. Tambahkan folder bin PostgreSQL ke PATH.' }

function New-DevSecret {
    $bytes = New-Object byte[] 32
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
    return [Convert]::ToBase64String($bytes)
}

function Invoke-DevSQL([string]$SQL, [string]$UserName, [string]$DatabaseName) {
    $result = $SQL | & $psql -X -w -h 127.0.0.1 -p $Port -U $UserName -d $DatabaseName -v ON_ERROR_STOP=1 -At
    if ($LASTEXITCODE -ne 0) { throw 'Setup PostgreSQL gagal; konfigurasi yang sudah dibuat tidak dihapus atau di-reset.' }
    return $result
}

Write-Host "Setup development lokal: PostgreSQL 127.0.0.1:$Port, admin PostgreSQL $AdminUser."
Write-Host 'Membuat database wbd_dev_platform, wbd_dev_target_a/b dan executor terpisah per target.'
Write-Host 'Objek lama dengan nama tersebut tidak akan ditimpa. Password admin PostgreSQL tidak disimpan.'
$adminPassword = Read-Host "Password PostgreSQL user $AdminUser" -AsSecureString
$previousPassword = $env:PGPASSWORD
try {
    $env:PGPASSWORD = (New-Object System.Net.NetworkCredential('', $adminPassword)).Password
    $existing = Invoke-DevSQL @'
SELECT rolname FROM pg_roles WHERE rolname IN ('wbd_dev_meta','wbd_dev_owner','wbd_dev_exec','wbd_dev_exec_a','wbd_dev_exec_b')
UNION ALL SELECT datname FROM pg_database WHERE datname IN ('wbd_dev_platform','wbd_dev_target_a','wbd_dev_target_b');
'@ $AdminUser 'postgres'
    if ($existing) { throw 'Nama role/database development sudah digunakan. Setup dihentikan tanpa mengubah objek yang ada; gunakan konfigurasi .env milik setup sebelumnya.' }
    $metaPassword = New-DevSecret
    $ownerPassword = New-DevSecret
    $passwordA = New-DevSecret
    $passwordB = New-DevSecret
    $key = New-DevSecret
    # Persist credentials before DDL so a partial setup never loses generated passwords.
    $config = @"
# Development only. Keep this file private; Git ignores it.
DATABASE_URL=postgres://wbd_dev_meta:$([uri]::EscapeDataString($metaPassword))@127.0.0.1:$Port/wbd_dev_platform?sslmode=disable
HTTP_ADDR=127.0.0.1:8080
APP_ORIGIN=http://127.0.0.1:3000
TARGET_CREDENTIAL_KEY=$key
FE_HTTP_ADDR=127.0.0.1:3000
FE_BACKEND_URL=http://127.0.0.1:8080
"@
    [System.IO.File]::WriteAllText($configPath, $config)
    $localDirectory = Join-Path $Root 'tmp/dev'
    New-Item -ItemType Directory -Path $localDirectory -Force | Out-Null
    [System.IO.File]::WriteAllText((Join-Path $localDirectory 'database-credentials.txt'), @"
Local development only. Do not commit or share this file.
Metadata role: wbd_dev_meta
Metadata password: $metaPassword
Owner role: wbd_dev_owner
Owner password: $ownerPassword

Add these two targets through the application's Connections page:
Host: 127.0.0.1
Port: $Port
Database: wbd_dev_target_a
Username: wbd_dev_exec_a
Password: $passwordA
SSL mode: disable (local development)

Database: wbd_dev_target_b
Username: wbd_dev_exec_b
Password: $passwordB
SSL mode: disable (local development)
"@)
    Invoke-DevSQL @"
BEGIN;
CREATE ROLE wbd_dev_meta LOGIN PASSWORD '$metaPassword';
CREATE ROLE wbd_dev_owner LOGIN PASSWORD '$ownerPassword';
CREATE ROLE wbd_dev_exec_a LOGIN PASSWORD '$passwordA';
CREATE ROLE wbd_dev_exec_b LOGIN PASSWORD '$passwordB';
COMMIT;
CREATE DATABASE wbd_dev_platform OWNER wbd_dev_meta;
CREATE DATABASE wbd_dev_target_a OWNER wbd_dev_owner;
CREATE DATABASE wbd_dev_target_b OWNER wbd_dev_owner;
GRANT CONNECT, CREATE ON DATABASE wbd_dev_target_a TO wbd_dev_exec_a;
GRANT CONNECT, CREATE ON DATABASE wbd_dev_target_b TO wbd_dev_exec_b;
"@ $AdminUser 'postgres' | Out-Null
    $env:PGPASSWORD = $ownerPassword
    foreach ($databaseName in @('wbd_dev_target_a', 'wbd_dev_target_b')) {
        $executor = if ($databaseName -eq 'wbd_dev_target_a') { 'wbd_dev_exec_a' } else { 'wbd_dev_exec_b' }
        & $psql -X -w -h 127.0.0.1 -p $Port -U wbd_dev_owner -d $databaseName -v ON_ERROR_STOP=1 -v "executor_role=$executor" -f (Join-Path $Root 'BE/sql/provision_target_history.sql') | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Provisioning $databaseName gagal. Credential tetap tersimpan; jangan hapus database." }
        Invoke-DevSQL "GRANT USAGE, CREATE ON SCHEMA public TO $executor;" 'wbd_dev_owner' $databaseName | Out-Null
    }
    Write-Host 'Database siap. Detail koneksi target tersimpan privat di tmp/dev/database-credentials.txt.'
} finally { $env:PGPASSWORD = $previousPassword }
