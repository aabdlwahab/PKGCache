# Build-container name resolution

A host entry installed by the client-onboarding script affects `docker pull` and
host-side tools. Dockerfile `RUN` steps have a separate hosts file and resolver.

The deterministic baseline is a per-build mapping:

```sh
docker build \
  --add-host pkgcache.internal=10.20.30.40 \
  --build-arg CACHE_NAME=pkgcache.internal \
  .
```

Compose has the equivalent under `build.extra_hosts`:

```sh
CACHE_IP=10.20.30.40 docker compose build
```

For fleet-wide resolution, start the optional forwarding resolver from the
repository root:

```sh
PKGCACHE_DNS_IP=10.20.30.40 docker compose --profile dns up -d dns
```

Then configure build hosts to use that address in `/etc/docker/daemon.json`:

```json
{
  "dns": ["10.20.30.40"]
}
```

Restart Docker after changing daemon configuration. A `docker-container` buildx
builder is a separate BuildKit daemon; configure its `[dns]` section using
`buildkitd.toml.example` rather than assuming it inherits `daemon.json`.

Always verify both the private name and an ordinary upstream name:

```sh
docker run --rm --dns 10.20.30.40 alpine:3.22 \
  sh -ec 'nslookup pkgcache.internal; nslookup example.com'
```

Site DNS remains preferable when available: one record reaches hosts, ordinary
containers, and builders without per-client daemon changes.
