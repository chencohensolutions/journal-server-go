param(
    [Parameter(Mandatory=$false)]
    [string]$CAKeyPath = "certs/ca-key.pem",

    [Parameter(Mandatory=$false)]
    [string]$CACertPath = "certs/ca.pem",

    [Parameter(Mandatory=$true)]
    [string]$CommonName,

    [Parameter(Mandatory=$false)]
    [string]$CertOutPath = "certs/$CommonName.pem",

    [Parameter(Mandatory=$false)]
    [string]$KeyOutPath = "certs/$CommonName-key.pem",

    [Parameter(Mandatory=$false)]
    [string]$P12OutPath = "certs/$CommonName.p12",

    [Parameter(Mandatory=$false)]
    [int]$Days = 365,

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

$csrPath = [System.IO.Path]::GetTempFileName()
try {
    Write-Host "Generating key for $CommonName..."
    & "$OpenSSLBinPath" genrsa -out "$KeyOutPath" 2048
    if ($LASTEXITCODE -ne 0) {
        throw "OpenSSL key generation failed with exit code $LASTEXITCODE"
    }

    Write-Host "Creating CSR for $CommonName..."
    & "$OpenSSLBinPath" req -new -key "$KeyOutPath" -out "$csrPath" -subj "/CN=$CommonName" -config "$OpenSSLConfigPath"
    if ($LASTEXITCODE -ne 0) {
        throw "OpenSSL CSR generation failed with exit code $LASTEXITCODE"
    }

    Write-Host "Signing certificate with CA..."
    & "$OpenSSLBinPath" x509 -req -in "$csrPath" -CA "$CACertPath" -CAkey "$CAKeyPath" -CAcreateserial -out "$CertOutPath" -days $Days -sha256
    if ($LASTEXITCODE -ne 0) {
        throw "Certificate signing failed with exit code $LASTEXITCODE"
    }

    Write-Host "Creating PKCS#12 bundle..."
    & "$OpenSSLBinPath" pkcs12 -export -out "$P12OutPath" -inkey "$KeyOutPath" -in "$CertOutPath"
    if ($LASTEXITCODE -ne 0) {
        throw "PKCS#12 bundle generation failed with exit code $LASTEXITCODE"
    }

    Write-Host "User certificate created: $CertOutPath"
    Write-Host "User private key created: $KeyOutPath"
    Write-Host "User PKCS#12 bundle created: $P12OutPath (no password)"
} finally {
    Remove-Item -Force "$csrPath" -ErrorAction SilentlyContinue
}
