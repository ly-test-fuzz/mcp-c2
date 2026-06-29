#!/usr/bin/env bash
set -euo pipefail

OUT_DIR=${1:-certs}
SERVER_NAME=${SERVER_NAME:-localhost}
CLIENT_NAME=${CLIENT_NAME:-mcp-c2-client}
DAYS=${DAYS:-3650}

mkdir -p "$OUT_DIR"
chmod 700 "$OUT_DIR"

openssl genrsa -out "$OUT_DIR/ca.key" 4096
openssl req -x509 -new -nodes -key "$OUT_DIR/ca.key" -sha256 -days "$DAYS" -out "$OUT_DIR/ca.crt" -subj "/CN=mcp-c2-ca"

openssl genrsa -out "$OUT_DIR/server.key" 4096
openssl req -new -key "$OUT_DIR/server.key" -out "$OUT_DIR/server.csr" -subj "/CN=$SERVER_NAME"
cat > "$OUT_DIR/server.ext" <<EOF
subjectAltName=DNS:$SERVER_NAME,DNS:localhost,IP:127.0.0.1
extendedKeyUsage=serverAuth
EOF
openssl x509 -req -in "$OUT_DIR/server.csr" -CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial -out "$OUT_DIR/server.crt" -days "$DAYS" -sha256 -extfile "$OUT_DIR/server.ext"

openssl genrsa -out "$OUT_DIR/client.key" 4096
openssl req -new -key "$OUT_DIR/client.key" -out "$OUT_DIR/client.csr" -subj "/CN=$CLIENT_NAME"
cat > "$OUT_DIR/client.ext" <<EOF
extendedKeyUsage=clientAuth
EOF
openssl x509 -req -in "$OUT_DIR/client.csr" -CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial -out "$OUT_DIR/client.crt" -days "$DAYS" -sha256 -extfile "$OUT_DIR/client.ext"

chmod 600 "$OUT_DIR"/*.key
printf 'client fingerprint sha256: '
openssl x509 -in "$OUT_DIR/client.crt" -noout -fingerprint -sha256 | cut -d= -f2 | tr -d ':' | tr 'A-F' 'a-f'
