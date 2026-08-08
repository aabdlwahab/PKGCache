#!/bin/sh
set -eu

if [ -z "${PKGCACHE_DNS_IP:-}" ]; then
  echo "dnsmasq: PKGCACHE_DNS_IP must be the cache address clients can reach" >&2
  exit 2
fi

case "$PKGCACHE_DNS_IP" in
  *[!0-9A-Fa-f:.]*)
    echo "dnsmasq: PKGCACHE_DNS_IP must be an IPv4 or IPv6 literal" >&2
    exit 2
    ;;
esac

CACHE_NAME="${PKGCACHE_DNS_NAME:-pkgcache.internal}"
case "$CACHE_NAME" in
  ""|*[!A-Za-z0-9.-]*)
    echo "dnsmasq: PKGCACHE_DNS_NAME is not a valid DNS name" >&2
    exit 2
    ;;
esac

exec dnsmasq --keep-in-foreground --conf-file=/etc/dnsmasq.conf \
  "--address=/${CACHE_NAME}/${PKGCACHE_DNS_IP}"
