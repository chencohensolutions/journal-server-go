#!/usr/bin/env bash
# Builds the Windows installer here: journal-server.exe into dist/, then dist/journal-server-setup-<version>.exe with
# Inno Setup in Docker (docs/install.md). On Windows, build the program the same way and run:
# ISCC.exe /DAppVersion=<version/VERSION> installer\journal-server.iss
# Published releases are built by .github/workflows/release.yml instead: a program built here doesn't update itself.
#
# usage: scripts/release.sh               (the version is version/VERSION, raised by the pre-commit hook)
set -euo pipefail
cd "$(dirname "$0")/.."

version="$(tr -d ' \r\n' < version/VERSION)"
if [ -n "$(git status --porcelain -- '*.go' go.mod go.sum)" ]; then
    echo "Uncommitted Go changes: commit them first, so this build's version ($version) names it." >&2
    exit 1
fi
# Inno Setup's image, pinned to a version (Inno Setup 6).
inno_image="amake/innosetup:innosetup6"

mkdir -p dist
build() {
    echo "Building dist/$1.exe"
    GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "dist/$1.exe" "$2"
}
build journal-server .

if ! command -v docker >/dev/null; then
    echo "Docker is needed to build the installer here (or run ISCC.exe on Windows); the programs are in dist/." >&2
    exit 1
fi
echo "Building dist/journal-server-setup-$version.exe"
docker run --rm -v "$PWD:/work" -w /work "$inno_image" \
    "/DAppVersion=$version" installer/journal-server.iss
ls -lh "dist/journal-server-setup-$version.exe"
