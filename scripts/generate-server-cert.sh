#!/usr/bin/env bash
# Generates the server's TLS key and certificate (certs/server-key.pem, certs/server.pem), signed by
# the CA from generate-ca.sh. Bash version of generate-server-cert.ps1.
#
# usage: scripts/generate-server-cert.sh [--cert-out PATH] [--key-out PATH] [--ca-cert PATH]
#                                        [--ca-key PATH] [--days N] [--openssl PATH]
#                                        [--openssl-config PATH]
#
# The certificate is valid for the names in alt_names below; devices must use one of them as the
# server address. OpenSSL is taken from --openssl, $OPENSSL_BIN, Homebrew, or PATH (in that order).
set -euo pipefail

cert_out="certs/server.pem"
key_out="certs/server-key.pem"
ca_cert="certs/ca.pem"
ca_key="certs/ca-key.pem"
days=365
openssl_bin="${OPENSSL_BIN:-}"
openssl_config="${OPENSSL_CONF:-}"

usage() {
    sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'
    exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --cert-out) cert_out="$2"; shift 2 ;;
        --key-out) key_out="$2"; shift 2 ;;
        --ca-cert) ca_cert="$2"; shift 2 ;;
        --ca-key) ca_key="$2"; shift 2 ;;
        --days) days="$2"; shift 2 ;;
        --openssl) openssl_bin="$2"; shift 2 ;;
        --openssl-config) openssl_config="$2"; shift 2 ;;
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

[[ -f "$ca_key" ]] || { echo "CA private key not found: $ca_key" >&2; exit 1; }
[[ -f "$ca_cert" ]] || { echo "CA certificate not found: $ca_cert" >&2; exit 1; }

config_args=()
if [[ -n "$openssl_config" ]]; then
    config_args=(-config "$openssl_config")
fi

mkdir -p "$(dirname "$cert_out")" "$(dirname "$key_out")"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

echo "Using $("$openssl_bin" version)"
echo "Generating server key..."
(umask 077 && "$openssl_bin" genrsa -out "$key_out" 2048)
chmod 600 "$key_out" # also when an existing file with looser permissions was overwritten

echo "Creating server CSR..."
"$openssl_bin" req -new -key "$key_out" -out "$work_dir/server.csr" -subj "/CN=localhost" \
    "${config_args[@]+"${config_args[@]}"}"

# iOS requires serverAuth and subjectAltName on TLS server certificates.
cat > "$work_dir/server.ext" <<'EOF'
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, nonRepudiation, keyEncipherment, dataEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names

[alt_names]
DNS.1 = localhost
DNS.2 = journal.example.com
IP.1 = 192.168.50.2
EOF

echo "Signing server certificate with CA..."
"$openssl_bin" x509 -req -in "$work_dir/server.csr" -CA "$ca_cert" -CAkey "$ca_key" -CAcreateserial \
    -out "$cert_out" -days "$days" -sha256 -extfile "$work_dir/server.ext"

"$openssl_bin" verify -CAfile "$ca_cert" "$cert_out" >/dev/null

echo "Server certificate created: $cert_out"
echo "Server private key created: $key_out"
