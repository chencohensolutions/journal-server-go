#!/usr/bin/env bash
# Generates the journal server's CA: certs/ca-key.pem (private key) and certs/ca.pem (certificate).
# Bash version of generate-ca.ps1.
#
# usage: scripts/generate-ca.sh [--cert-out PATH] [--key-out PATH] [--days N]
#                               [--openssl PATH] [--openssl-config PATH] [--force]
#
# OpenSSL is taken from --openssl, $OPENSSL_BIN, Homebrew, or PATH (in that order) and must be
# OpenSSL 1.1.1+ or a LibreSSL that supports -addext.
set -euo pipefail

cert_out="certs/ca.pem"
key_out="certs/ca-key.pem"
days=3650
openssl_bin="${OPENSSL_BIN:-}"
openssl_config="${OPENSSL_CONF:-}"
force=false

usage() {
    sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
    exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --cert-out) cert_out="$2"; shift 2 ;;
        --key-out) key_out="$2"; shift 2 ;;
        --days) days="$2"; shift 2 ;;
        --openssl) openssl_bin="$2"; shift 2 ;;
        --openssl-config) openssl_config="$2"; shift 2 ;;
        --force) force=true; shift ;;
        -h|--help) usage 0 ;;
        *) echo "Unknown option: $1" >&2; usage 1 ;;
    esac
done

if [[ -z "$openssl_bin" ]]; then
    for candidate in /opt/homebrew/bin/openssl /usr/local/opt/openssl/bin/openssl; do
        if [[ -x "$candidate" ]]; then
            openssl_bin="$candidate"
            break
        fi
    done
fi
if [[ -z "$openssl_bin" ]]; then
    openssl_bin="$(command -v openssl || true)"
fi
if [[ -z "$openssl_bin" ]]; then
    echo "Could not find openssl. Install OpenSSL or pass --openssl with its path." >&2
    exit 1
fi

# A new CA invalidates every certificate signed by the old one (server and devices).
if [[ "$force" != true && ( -e "$cert_out" || -e "$key_out" ) ]]; then
    echo "$cert_out or $key_out already exists. Replacing the CA invalidates the server certificate and" >&2
    echo "every enrolled device; pass --force to do it anyway." >&2
    exit 1
fi

config_args=()
if [[ -n "$openssl_config" ]]; then
    config_args=(-config "$openssl_config")
fi

mkdir -p "$(dirname "$cert_out")" "$(dirname "$key_out")"

echo "Using $("$openssl_bin" version)"
echo "Generating CA key..."
(umask 077 && "$openssl_bin" genrsa -out "$key_out" 4096)
chmod 600 "$key_out" # also when an existing file with looser permissions was overwritten

echo "Generating CA certificate..."
# Mark the certificate as a CA explicitly instead of relying on openssl.cnf defaults: without these
# extensions OpenSSL may write a version 1 certificate, which iOS refuses as a trust anchor.
"$openssl_bin" req -x509 -new -nodes -key "$key_out" -sha256 -days "$days" -out "$cert_out" \
    -subj "/CN=journal-server-ca" "${config_args[@]+"${config_args[@]}"}" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -addext "subjectKeyIdentifier=hash"

if ! "$openssl_bin" x509 -in "$cert_out" -noout -text | grep -q "CA:TRUE"; then
    echo "The generated certificate $cert_out is not marked as a CA (CA:TRUE missing)." >&2
    echo "Use OpenSSL 1.1.1 or newer (--openssl PATH)." >&2
    exit 1
fi

echo "CA certificate created: $cert_out"
echo "CA private key created: $key_out"
