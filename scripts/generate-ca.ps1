param(
    [Parameter(Mandatory=$false)]
    [string]$CertOutPath = "certs/ca.pem",

    [Parameter(Mandatory=$false)]
    [string]$KeyOutPath = "certs/ca-key.pem",

    [Parameter(Mandatory=$false)]
    [int]$Days = 3650,

    [Parameter(Mandatory=$false)]
    [string]$OpenSSLConfig,

    [Parameter(Mandatory=$false)]
    [string]$OpenSSLPath
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

New-Item -ItemType Directory -Path (Split-Path $CertOutPath) -Force | Out-Null
New-Item -ItemType Directory -Path (Split-Path $KeyOutPath) -Force | Out-Null

Write-Host "Generating CA key..."
& "$OpenSSLBinPath" genrsa -out "$KeyOutPath" 4096
if ($LASTEXITCODE -ne 0) {
    throw "OpenSSL CA key generation failed with exit code $LASTEXITCODE"
}

Write-Host "Generating CA certificate..."
& "$OpenSSLBinPath" req -x509 -new -nodes -key "$KeyOutPath" -sha256 -days $Days -out "$CertOutPath" -subj "/CN=journal-server-ca" -config "$OpenSSLConfigPath"
if ($LASTEXITCODE -ne 0) {
    throw "OpenSSL CA certificate generation failed with exit code $LASTEXITCODE"
}

Write-Host "CA certificate created: $CertOutPath"
Write-Host "CA private key created: $KeyOutPath"
