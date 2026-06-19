#!/usr/bin/env bash
# Mint the TLS material the Caddy reverse proxy serves (config/Caddyfile), so the
# cache's registry/index services (zot, Verdaccio, devpi) are reachable over
# HTTPS instead of plain HTTP.
#
#   ./scripts/gen-certs.sh [extra-hostname-or-IP ...]
#
# Produces under certs/:
#   ca.crt / ca.key       — a private CA (reused if it already exists, so trust
#                           you've already distributed stays valid)
#   server.crt/server.key — the proxy's cert, signed by that CA
#
# "Automatically trusted" means: install certs/ca.crt into each build host's
# trust store ONCE (see the printout at the end). After that, docker/apt/apk
# trust the cache with no insecure flags; pip/npm need the CA pointed at via env
# (also printed). The CA must travel across the air gap too — it's safe to share
# (it contains no private key); ca.key and server.key must NOT leave this host.
set -euo pipefail
cd "$(dirname "$0")/.."

CERTS="certs"
mkdir -p "$CERTS"
DAYS="${CERT_DAYS:-3650}"   # long-lived by default: an air-gapped side has no renewal path

# --- Subject Alternative Names ------------------------------------------------
# A cert is trusted for a name only if that name is in its SANs. Cover the names
# clients actually use to reach the cache: loopback, this host's name + primary
# IP, and anything extra passed as arguments (other build hosts' view of it).
declare -A _seen
SANS=""
add_san() {
  local v="${1:-}"; [ -z "$v" ] && return
  local kind="DNS"
  [[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] && kind="IP"
  local entry="$kind:$v"
  [ -n "${_seen[$entry]:-}" ] && return
  _seen[$entry]=1
  SANS="${SANS:+$SANS,}$entry"
}
add_san localhost
add_san 127.0.0.1
add_san "$(hostname)"
add_san "$(hostname -f 2>/dev/null || true)"
add_san "$(hostname -I 2>/dev/null | awk '{print $1}')"
for x in "$@"; do add_san "$x"; done

# --- CA (create once, then reuse) ---------------------------------------------
if [ -f "$CERTS/ca.crt" ] && [ -f "$CERTS/ca.key" ]; then
  echo "==> reusing existing CA ($CERTS/ca.crt) so already-distributed trust stays valid"
else
  echo "==> generating private CA"
  openssl genrsa -out "$CERTS/ca.key" 4096
  openssl req -x509 -new -nodes -key "$CERTS/ca.key" -sha256 -days "$DAYS" \
    -subj "/CN=package-cache local CA/O=package-cache" -out "$CERTS/ca.crt"
fi

# --- Server cert signed by the CA ---------------------------------------------
echo "==> generating server cert for SANs: $SANS"
openssl genrsa -out "$CERTS/server.key" 2048
openssl req -new -key "$CERTS/server.key" -subj "/CN=$(hostname)" -out "$CERTS/server.csr"
cat > "$CERTS/server.ext" <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=$SANS
EOF
openssl x509 -req -in "$CERTS/server.csr" -CA "$CERTS/ca.crt" -CAkey "$CERTS/ca.key" \
  -CAcreateserial -days "$DAYS" -sha256 -extfile "$CERTS/server.ext" \
  -out "$CERTS/server.crt"
rm -f "$CERTS/server.csr" "$CERTS/server.ext"
chmod 600 "$CERTS/ca.key" "$CERTS/server.key"

cat <<EOF

==> done. certs/ now holds ca.crt, ca.key, server.crt, server.key
    (Caddy reads server.{crt,key}; ca.key/server.key stay on this host only.)

To make a build host trust the cache (one-time), copy certs/ca.crt over and:

  # System store -> docker, apt, apk trust it with NO insecure flags
  sudo cp ca.crt /usr/local/share/ca-certificates/package-cache.crt   # Debian/Ubuntu
  sudo update-ca-certificates
  #   (RHEL/Alpine: /etc/pki/ca-trust/source/anchors/ + update-ca-trust,
  #    or /etc/apk/keys is NOT for TLS — Alpine uses ca-certificates too)

  # pip and npm do NOT use the system store — point them at the CA:
  export PIP_CERT=/path/to/ca.crt
  npm config set cafile /path/to/ca.crt     # or: export NODE_EXTRA_CA_CERTS=/path/to/ca.crt

See docs/docker-builds.md for the in-Dockerfile versions of the above.
EOF
