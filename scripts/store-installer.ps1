# Makes the signed installer for the Microsoft Store from a published release, on Windows (docs/install.md,
# "Microsoft Store"). The code-signing certificate must be reachable by signtool: for Certum's cloud certificates,
# SimplySign Desktop connected, which puts the certificate in the Windows certificate store.
#
#   scripts\store-installer.ps1 -Version 2026.11.18 [-Certificate "Open Source Developer, Hen Cohen"] [-Upload]
#
# 1. Downloads the release's journal-server-windows-amd64.exe (a release build, so it updates itself) and checks it
#    against the release's SHA256SUMS.
# 2. Puts ONNX Runtime, libvips and ffmpeg beside it (scripts/bundle-libraries), so the installer works offline.
# 3. Signs journal-server.exe, then builds the installer with Inno Setup, which signs the installer and its
#    uninstaller (/DSign).
# 4. Checks the signatures and, with -Upload, adds dist\store\journal-server-setup-<version>-signed.exe to the
#    release (needs the gh command, signed in). That file's URL is what Partner Center gets: it never changes.
#
# Needs Go, Inno Setup 6 and signtool (Windows SDK). Run from the repository, at the release's commit.
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [string]$Certificate = "",
    [string]$Timestamp = "http://time.certum.pl",
    [switch]$Upload
)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

$repo = "chencohensolutions/journal-server-go"
$base = "https://github.com/$repo/releases/download/v$Version"
$out = "dist\store"
Remove-Item -Recurse -Force $out -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force $out | Out-Null

$signtool = Get-ChildItem "${env:ProgramFiles(x86)}\Windows Kits\10\bin\*\x64\signtool.exe" -ErrorAction SilentlyContinue |
    Sort-Object FullName | Select-Object -Last 1 -ExpandProperty FullName
if (-not $signtool) { throw "signtool.exe not found: install the Windows SDK's signing tools (winget install Microsoft.WindowsSDK.10.0.26100)" }
$iscc = "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe"
if (-not (Test-Path $iscc)) { throw "Inno Setup 6 not found: winget install JRSoftware.InnoSetup" }

# The certificate: by its subject name when given, else the best one signtool finds.
$pick = if ($Certificate) { "/n `"$Certificate`"" } else { "/a" }
$signArgs = "sign $pick /fd sha256 /tr $Timestamp /td sha256"

Write-Host "Downloading the release's program"
Invoke-WebRequest "$base/journal-server-windows-amd64.exe" -OutFile "$out\journal-server.exe"
Invoke-WebRequest "$base/SHA256SUMS" -OutFile "$out\SHA256SUMS"
$want = (Select-String -Path "$out\SHA256SUMS" -Pattern "^([0-9a-f]{64})\s+\*?journal-server-windows-amd64\.exe$").Matches[0].Groups[1].Value
$got = (Get-FileHash "$out\journal-server.exe" -Algorithm SHA256).Hash.ToLower()
if ($want -ne $got) { throw "journal-server-windows-amd64.exe doesn't match the release's SHA256SUMS" }
Remove-Item "$out\SHA256SUMS"

Write-Host "Putting the libraries beside it"
go run ./scripts/bundle-libraries --out $out
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "Signing journal-server.exe"
& $signtool $signArgs.Split(" ", [StringSplitOptions]::RemoveEmptyEntries) "$out\journal-server.exe"
if ($LASTEXITCODE -ne 0) { throw "signtool failed on journal-server.exe" }

Write-Host "Building and signing the installer"
# Inno Setup runs the sign tool with $f for the file and $q for a quote.
$innoSign = "/Sjournal=`$q$signtool`$q " + ($signArgs -replace '"', '$q') + ' $f'
& $iscc "/DAppVersion=$Version" "/DDist=..\$out" "/DSign" $innoSign installer\journal-server.iss
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

$setup = "$out\journal-server-setup-$Version-signed.exe"
Move-Item "$out\journal-server-setup-$Version.exe" $setup
& $signtool verify /pa /v $setup | Out-Null
if ($LASTEXITCODE -ne 0) { throw "the installer's signature doesn't verify" }
Write-Host "Signed: $setup ($([math]::Round((Get-Item $setup).Length / 1MB)) MB)"

if ($Upload) {
    gh release upload "v$Version" $setup --repo $repo
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    Write-Host "For Partner Center: $base/journal-server-setup-$Version-signed.exe"
}
