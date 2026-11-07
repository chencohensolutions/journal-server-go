param(
    [Parameter(Mandatory=$false)]
    [string]$CertOutPath = "certs/server.pem",

    [Parameter(Mandatory=$false)]
    [string]$KeyOutPath = "certs/server-key.pem",

    [Parameter(Mandatory=$false)]
    [string]$CAKeyPath = "certs/ca-key.pem",

    [Parameter(Mandatory=$false)]
    [string]$CACertPath = "certs/ca.pem",

    [Parameter(Mandatory=$false)]
    [int]$Days = 365,

    [Parameter(Mandatory=$false)]
    [string]$OpenSSLConfig,

    [Parameter(Mandatory=$false)]
    [string]$OpenSSLPath,

    # Your own names and IP addresses for the certificate, beside localhost and the LAN addresses
    # below (default: $env:SERVER_NAMES, spaces or commas between them), e.g. "myhome.duckdns.org 10.0.0.5".
    [Parameter(Mandatory=$false)]
    [string]$ServerNames = $env:SERVER_NAMES
)

$ErrorActionPreference = 'Stop'

function Get-OpenSSLConfigPath {
    if ($OpenSSLConfig) {
        return $OpenSSLConfig
    }
    if ($env:OPENSSL_CONF) {
        return $env:OPENSSL_CONF
    }

    $candidates = @(
        "$env:ProgramFiles\Common Files\SSL\openssl.cnf",
        "$env:ProgramFiles(x86)\Common Files\SSL\openssl.cnf",
        "$env:ProgramFiles\Git\mingw64\ssl\openssl.cnf",
        "$env:ProgramFiles\Git\usr\ssl\openssl.cnf"
    )

    foreach ($path in $candidates) {
        if ($path -and (Test-Path $path)) {
            return $path
        }
    }

    throw "Could not find openssl.cnf. Set the OPENSSL_CONF environment variable or pass -OpenSSLConfig with the path to openssl.cnf."
}

function Get-OpenSSLBinaryPath {
    if ($OpenSSLPath) {
        return $OpenSSLPath
    }
    if ($env:OPENSSL_BIN) {
        return $env:OPENSSL_BIN
    }

    $candidates = @(
        "$env:ProgramFiles\OpenSSL-Win64\bin\openssl.exe",
        "$env:ProgramFiles\OpenSSL-Win32\bin\openssl.exe",
        "$env:ProgramFiles(x86)\OpenSSL-Win32\bin\openssl.exe",
        "$env:ProgramFiles\Git\mingw64\bin\openssl.exe",
        "$env:ProgramFiles\Git\usr\bin\openssl.exe"
    )

    foreach ($path in $candidates) {
        if ($path -and (Test-Path $path)) {
            return $path
        }
    }

    $cmd = Get-Command openssl -ErrorAction SilentlyContinue
    if ($cmd) {
        return $cmd.Source
    }

    throw "Could not find openssl binary. Install OpenSSL and ensure it is on PATH or pass -OpenSSLPath with the path to openssl.exe."
}

$OpenSSLConfigPath = Get-OpenSSLConfigPath
$OpenSSLBinPath = Get-OpenSSLBinaryPath

if (-not (Test-Path $CAKeyPath)) {
    throw "CA private key not found: $CAKeyPath"
}
if (-not (Test-Path $CACertPath)) {
    throw "CA certificate not found: $CACertPath"
}

New-Item -ItemType Directory -Path (Split-Path $CertOutPath) -Force | Out-Null
New-Item -ItemType Directory -Path (Split-Path $KeyOutPath) -Force | Out-Null

$csrPath = [System.IO.Path]::GetTempFileName()
$extPath = [System.IO.Path]::GetTempFileName()

try {
    Write-Host "Generating server key..."
    & "$OpenSSLBinPath" genrsa -out "$KeyOutPath" 2048
    if ($LASTEXITCODE -ne 0) {
        throw "OpenSSL server key generation failed with exit code $LASTEXITCODE"
    }

    Write-Host "Creating server CSR..."
    & "$OpenSSLBinPath" req -new -key "$KeyOutPath" -out "$csrPath" -subj "/CN=localhost" -config "$OpenSSLConfigPath"
    if ($LASTEXITCODE -ne 0) {
        throw "OpenSSL CSR generation failed with exit code $LASTEXITCODE"
    }

    Write-Host "Creating extension file with SAN..."
    $extContent = @"
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, nonRepudiation, keyEncipherment, dataEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names

[alt_names]
DNS.1 = localhost
IP.1 = 192.168.50.2
IP.2 = 192.168.50.150
"@
    $dnsN = 2; $ipN = 3
    foreach ($name in ($ServerNames -split '[,\s]+' | Where-Object { $_ })) {
        if ($name -match '^[0-9.]+$' -or $name.Contains(':')) {
            $extContent += "`nIP.$ipN = $name"; $ipN++
        } else {
            $extContent += "`nDNS.$dnsN = $name"; $dnsN++
        }
    }
    Set-Content -Path $extPath -Value $extContent

    Write-Host "Signing server certificate with CA..."
    & "$OpenSSLBinPath" x509 -req -in "$csrPath" -CA "$CACertPath" -CAkey "$CAKeyPath" -CAcreateserial -out "$CertOutPath" -days $Days -sha256 -extfile "$extPath"
    if ($LASTEXITCODE -ne 0) {
        throw "Certificate signing failed with exit code $LASTEXITCODE"
    }

    Write-Host "Server certificate created: $CertOutPath"
    Write-Host "Server private key created: $KeyOutPath"
} finally {
    Remove-Item -Force "$csrPath" -ErrorAction SilentlyContinue
    Remove-Item -Force $extPath -ErrorAction SilentlyContinue
}
