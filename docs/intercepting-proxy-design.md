# Design: TLS-intercepting cache proxy ("option F")

Status: **proposal, not built.** This document exists to be argued with before any code
is written, because the decision it asks for is not a technical one.

## 0. What is being proposed, in one paragraph

pkgreg would answer `CONNECT` on its forward-proxy listener, present a certificate it
minted itself for the host the client asked for, terminate that TLS session, and serve
the request from cache. A build would then need no index URL, no `ARG` block, no
flavoured base image and no Dockerfile change: `pip install six` reaches
`https://pypi.org` as far as pip is concerned, and pkgreg answers.

## 1. The decision this asks for

Every other option in this space changes *configuration*. This one changes **what the
cache is**.

Today pkgreg is a thing clients are pointed at. Its CA signs one identity — the cache's
own — and the fingerprint a developer verifies proves exactly that. After this change,
the cache would hold a key that can produce a certificate for `pypi.org`, and every
machine that trusts it would accept that certificate as genuine. The cache stops being
a service you talk to and becomes a party in the middle of conversations that the
software on both ends believes are private.

That is a legitimate, common thing for an organisation to do inside its own
infrastructure. It is also the single largest expansion of blast radius in this
product's history, and it should be adopted deliberately and named honestly, not
introduced as a convenience feature. Everything below is written to make the resulting
system as narrow and as auditable as possible.

## 2. Threat model

### 2.1 What an attacker gains if the cache host is compromised

| | Today | With interception |
|---|---|---|
| Serve a bad package on a request routed to the cache | yes | yes |
| Impersonate the cache's own hostname | yes | yes |
| Impersonate `pypi.org`, `registry.npmjs.org`, … to every trusting client | **no** | yes, for allowlisted hosts |
| Read credentials clients send to those upstreams | no | yes, unless deliberately discarded |
| Impersonate arbitrary third parties (a bank, an SSO endpoint) | no | **no** — prevented by §4.3 name constraints |

The third row is the real change. The fifth is the control that keeps it from being
unbounded, and it is the reason §4.3 is not optional.

### 2.2 Attacker classes considered

1. **Network attacker between client and cache.** Unchanged: the hop is still TLS to
   the cache, still pinned by fingerprint on first setup.
2. **Compromised cache host.** Materially worse, as above. Mitigated by name
   constraints (§4.3), an allowlist that cannot be widened at runtime (§6), separate
   key material (§4.1), and a revocation path that does not require touching the
   existing CA (§4.6).
3. **Malicious insider with cache operator access.** Same as (2). Mitigated
   additionally by audit (§10) — every enable, every allowlist change and every CA
   mint is an audit record, and the audit log is superuser-only.
4. **Compromised client.** A client that trusts the interception CA can be lied to
   about allowlisted hosts. It could already be lied to about packages, so this is a
   change in degree, not in kind — except that it now extends to any content served
   from those hosts, not only package paths.
5. **Curious operator.** Interception makes request contents readable. §9 exists to
   ensure the product never records them.

### 2.3 What is explicitly out of scope

- Intercepting anything not on the allowlist.
- Intercepting user browser traffic. This runs on the package proxy listener, which is
  not a general egress proxy and must never be advertised as one.
- Inspecting or filtering content for policy reasons. This is a cache, not a DLP
  appliance. Adding "while we're in the middle, let's also scan" is how a narrow
  mechanism becomes an unbounded one; it should require its own decision.

## 3. Non-negotiable constraints

These are the spine. If any of them cannot be met, the feature should not ship.

1. **Separate trust anchor.** Interception uses its own CA, never the CA that signs the
   cache's own certificate (§4.1).
2. **Name-constrained.** The interception CA carries X.509 name constraints limiting it
   to the exact hosts it may impersonate (§4.3).
3. **Positive allowlist only.** Derived from upstreams the operator already configured.
   No wildcard. No "intercept everything". No runtime widening from a request (§6).
4. **Off by default, with a multi-step opt-in** that cannot be satisfied by accident
   (§11.1).
5. **Default-deny for the unknown.** A `CONNECT` to a host that is not allowlisted is
   either refused or blind-tunnelled by explicit configuration — never intercepted
   (§6.4).
6. **Strict upstream verification, no fallback.** The cache verifies the real upstream
   against the system roots. A verification failure is a failure; it never degrades to
   an unverified fetch (§7.3).
7. **Credentials are not observed.** Anything that looks like an authorization
   credential in intercepted traffic is never logged, never stored, never cached, and
   never appears in audit detail (§9).
8. **Interception is discoverable.** A client can determine that it is happening, and
   the operator surface says so plainly (§10.4).

## 4. The interception CA

### 4.1 It must be a separate root

Two independent reasons, one of which is already true in the code:

- `internal/pki/pki.go` mints the existing root with `MaxPathLen: 0,
  MaxPathLenZero: true`. It **cannot sign an intermediate CA.** Making it able to would
  mean re-issuing the root, invalidating trust on every machine that already has it.
- More importantly, we would not want to even if we could. The existing CA's meaning is
  "this is the cache". Its fingerprint is read out over a trusted channel precisely to
  establish that. Giving that same anchor the power to mint `pypi.org` retroactively
  changes what every already-verified fingerprint attests to.

So: a second root, `mitm-ca.crt` / `mitm-ca.key`, with its own fingerprint, its own
distribution, and its own lifecycle. A machine can trust the cache without trusting the
interceptor. That separation is the feature.

### 4.2 Key and algorithm

- **ECDSA P-256** for the interception root and all leaves, not RSA-4096 as the
  existing root uses. Leaf minting happens on the request path; a 2048-bit RSA keygen
  is tens of milliseconds and a trivially cheap denial-of-service lever, while P-256 is
  microseconds. Universally supported by every client in scope.
- Root validity **1 year**, not the existing root's 10. This key is far more dangerous
  and should expire on its own if forgotten. Renewal is a documented operator step.
- Generated only by an explicit command; never created implicitly at startup, unlike
  `LoadOrCreateCA`.

### 4.3 Name constraints — the central control

The interception root is issued with `PermittedDNSDomains` set to exactly the hosts on
the allowlist, and `PermittedDNSDomainsCritical: true`.

```
X509v3 Name Constraints: critical
    Permitted:
      DNS:pypi.org
      DNS:files.pythonhosted.org
      DNS:registry.npmjs.org
```

This is enforced by the verifier, not by us. Go's `crypto/x509`, OpenSSL and NSS all
enforce name constraints on a trust anchor. The consequence is the one that matters:
**if this key is stolen outright, it still cannot mint a certificate for anything
outside that list.** It cannot impersonate a bank, an identity provider, or a corporate
service. The damage is bounded by the certificate itself, not by our code being correct.

Additionally set `ExcludedDNSDomains` for the operator's own internal domains, so a
misconfigured allowlist cannot be used to impersonate internal services. Marking the
extension critical means a client that does not understand name constraints rejects the
certificate entirely, which is the correct failure direction.

Changing the allowlist therefore requires **re-issuing the interception root** and
redistributing it. This is deliberate friction: widening the set of impersonable hosts
should be an event, not a config reload.

### 4.4 Storage

- `<data-dir>/certs/mitm-ca.key`, mode `0600`, owned by the service user.
- Never included in air-gap export bundles. The existing code already excludes the
  ordinary CA key from export; this must be excluded by the same mechanism and covered
  by a test that fails if it ever appears in an export.
- Never returned by any API. `/api/ca.crt` serves the ordinary CA; the interception
  root gets a **separate** endpoint serving only the certificate, never the key.
- Loaded into memory once at startup; the file is not re-read per request.

### 4.5 Distribution

The interception root has to reach client trust stores, which is the same problem the
ordinary CA already solved, so it reuses the same machinery — `pkgreg-client
--persist`, `-docker-trust`, and the generated `setup.sh` — but as a **separate,
explicitly-named artifact with its own fingerprint**. The client must:

- print, in plain language, that this certificate allows the cache to present itself as
  the listed third-party hosts, and list them, read out of the certificate's own name
  constraints rather than from a claim made alongside it;
- require a distinct flag (`-accept-interception`) that has no default;
- refuse to install it silently as part of ordinary setup.

### 4.6 Rotation and revocation

- Rotation is `pkgreg mitm-ca rotate`: mint a new root, keep serving leaves from the old
  one for a grace period so builds do not break mid-flight, then stop.
- **The kill switch is `pkgreg mitm disable`,** which stops interception immediately
  and independently of certificate expiry. It must not require clients to do anything.
- Deleting `mitm-ca.key` must be safe: the server logs, refuses to intercept, and
  tunnels or refuses per policy. It must never fall back to serving an unverified
  connection.
- No CRL/OCSP. They would not be checked by the clients in scope, and pretending
  otherwise is worse than not offering them. Revocation is "turn it off and rotate",
  which is honest.

## 5. Leaf minting

- Triggered only by an allowlisted `CONNECT` whose SNI matches the requested host.
- **SAN is exactly the one requested host.** No wildcards, ever — a `*.pythonhosted.org`
  leaf is a smaller version of the same problem name constraints exist to solve.
- Validity **24 hours**, `NotBefore` backdated 5 minutes for clock skew.
- `ExtKeyUsage: ServerAuth` only. `KeyUsage: DigitalSignature | KeyEncipherment`.
  `IsCA: false`, `BasicConstraintsValid: true`.
- Random 128-bit serial, from `crypto/rand`, per certificate.
- **One key pair per host, not per connection**, cached with the certificate. Reusing
  one key across all hosts would mean a single leaked leaf key compromises every
  intercepted identity.
- **Cache in memory only.** Leaves are never written to disk: they are cheap to
  regenerate and a directory full of third-party certificates is an artifact nobody
  should have to explain in an audit.
- Bounded LRU (e.g. 256 entries) keyed by host, with a hard mint-rate limit per host
  and globally. Without this, a client can drive unbounded signing work by varying SNI.
- Minting is refused for anything that parses as an IP address, a bare label, or a
  name not in the constraint set — belt and braces with §4.3.

## 6. What may be intercepted

### 6.1 Derived from configuration that already exists

The allowlist is not a new hand-maintained list. It is computed from the **upstreams
the operator has already configured** (`control.Upstream.URL`), plus the ecosystem
defaults (`https://pypi.org/simple`, `https://registry.npmjs.org`, the PyTorch indexes).
If pkgreg is not configured to fetch from a host, it has no business impersonating it.

Two additions are needed and must be explicit, because indexes redirect to separate
download hosts:

- pypi's `files.pythonhosted.org`
- npm's tarball host, if it differs from the registry host

These are declared in configuration per ecosystem, not discovered from a redirect at
runtime. **Discovering an interceptable host from a response would let an upstream
choose what we impersonate**, which is a remote-controlled widening of our own trust
boundary.

### 6.2 Operator confirmation

The derived set is *proposed*; the operator must confirm it. `pkgreg mitm enable`
prints the exact list and requires it to be echoed back or passed explicitly. The
confirmed list is what goes into the name constraints, so the certificate and the
policy cannot drift apart.

### 6.3 Project scoping

The proxy already selects a project from the proxy username (`project@host:port`,
`router.ProxyProject`). Interception is enabled **per project**, so a team can adopt it
without imposing it on every tenant of a shared cache. A project with interception off
gets §6.4 behaviour.

### 6.4 Everything else

For a `CONNECT` to a host that is not on the confirmed list, exactly one of two
configured behaviours, defaulting to the first:

- **`refuse`** — answer `403` with a message naming the host. Nothing is tunnelled;
  pkgreg is not an egress proxy.
- **`tunnel`** — a blind TCP relay, no interception, no caching, no inspection, subject
  to the existing `ProxyHostAllowed` allowlist. This is "option F-lite" and is useful
  on its own: it makes `HTTPS_PROXY` safe to set globally, which today breaks every
  https fetch in a build.

`tunnel` must never be the default and must be logged at startup, because an open
tunnel is an egress path out of a build network.

## 7. TLS handling

### 7.1 Client-facing side

- Minimum **TLS 1.2**; prefer 1.3. No fallback, no legacy cipher suites.
- **SNI is required.** A `CONNECT` with no SNI on the inner handshake is refused, not
  guessed from the `CONNECT` target — the two disagreeing is a signal, not a nuisance.
- The SNI must equal the `CONNECT` host. A mismatch is refused and logged.
- ALPN is negotiated and **echoed from what the upstream actually agreed**, not assumed.
  Offering `h2` and then speaking HTTP/1.1 upstream produces failures that look like
  cache corruption.
- Session tickets: separate ticket keys per intercepted identity, or disabled. A ticket
  issued for one identity must never resume under another.

### 7.2 Upstream side

- A **separate `http.Client`/transport per upstream host**, verifying against the
  system root pool — explicitly *not* the pkgreg CA pool, and never
  `InsecureSkipVerify`. This is the highest-value assertion in the whole design and
  needs a test that fails if anyone ever adds a skip flag.
- SNI set to the real host. Upstream credentials, where the project has them
  configured, are attached by the existing credential machinery — never forwarded from
  what the client happened to send (§9.2).

### 7.3 Failure is failure

If upstream verification fails, the intercepted connection fails. It must never fall
back to an unverified fetch, never serve a stale cached copy as though it were fresh,
and never silently switch to tunnelling. The failure is surfaced to the client as a
gateway error naming the upstream, and recorded.

### 7.4 Pinning

Tools that pin certificates or public keys will break, correctly, and there is no way
around that short of not intercepting them. This is documented rather than worked
around. `pkgreg doctor` should probe the configured upstreams and warn about any known
to pin.

## 8. Request handling

Once the inner TLS session is established, the plaintext request is handled by the
existing ecosystem adapters, selected by host → ecosystem mapping derived from the same
upstream configuration as §6.1.

One genuinely nice consequence: **URL rewriting disappears.** Today the npm adapter must
rewrite tarball URLs so clients do not walk past the cache (`Ctx.ExternalBase`). Under
interception the client believes it is talking to `registry.npmjs.org`, so the original
URLs are correct as-is — provided the tarball host is also intercepted. That makes §6.1's
"declare the download hosts explicitly" a correctness requirement, not just a
convenience.

Non-package paths on an intercepted host (anything the adapter does not recognise) are
**proxied without caching**, or refused. They must not be cached: the cache's storage
model is keyed on package semantics, and storing arbitrary paths from an impersonated
host is how a cache becomes an unintended web archive.

## 9. Data handling

This section is what keeps "we can read everything" from becoming "we do read
everything".

### 9.1 Never recorded

For intercepted requests, the following must never reach logs, the audit table, the
job log, metrics labels, or an error message:

- `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`
- query strings on non-package paths
- request bodies, in any form, at any log level
- response bodies other than through the normal cache-storage path

Implemented as a positive-list redactor applied at the boundary, not as a blocklist
sprinkled through call sites. A blocklist misses the header someone adds next year.

### 9.2 Client credentials are dropped, not forwarded

If a client sends its own upstream credentials (a `.netrc`, an npm token), the cache
must **not** forward them and must not store them. It uses the project's configured
credential or none. Forwarding would make the cache a credential relay and make a cache
compromise a credential compromise for every developer.

If a request is rejected upstream for lack of credentials, the answer is "configure a
credential on this project", not "pass through whatever the client had".

### 9.3 Caching rules

Only what the ecosystem adapters already cache, under the same freshness rules. No new
caching behaviour is introduced by interception.

## 10. Observability

### 10.1 Recorded

- Startup line stating interception is enabled, listing every host and the resolved
  policy for the unknown case.
- An audit record for: enabling, disabling, allowlist change, CA mint, CA rotation.
  These are superuser-only reads, as the audit log already is.
- Per-request metrics by host and outcome — counts only, no paths, no identifiers.

### 10.2 Not recorded

Per-request logs naming a package or path for intercepted traffic are off by default.
The stats page already reports what was cached, at package granularity, through the
normal path.

### 10.3 `pkgreg doctor`

Fails or warns on: interception enabled with no name constraints; `tunnel` policy
configured; interception root within 30 days of expiry; interception root key readable
by anyone but the service user; interception enabled while the ordinary CA and the
interception CA are the same file.

### 10.4 Discoverability

The console shows interception status prominently on the project, and the tutorial says
in plain words what a machine trusting the interception CA is accepting. A developer
should never discover this by reading a certificate chain.

## 11. Operator and developer experience

### 11.1 Operator, once on the cache host

```
$ pkgreg mitm-ca init
```

```
Interception CA minted.

  fingerprint   SHA256:9F:2C:...:44
  valid until   2027-08-06  (1 year — renewal is a deliberate step)
  key           <data-dir>/certs/mitm-ca.key   (0600, never exported)

  This certificate can answer as, and as nothing else:
      pypi.org
      files.pythonhosted.org
      registry.npmjs.org

  A machine that trusts it cannot tell this cache from those hosts. The list
  above is written into the certificate as a constraint and is enforced by the
  client, not by this server: with this key, nothing else on the internet can
  be impersonated. Changing the list means re-issuing this certificate and
  redistributing it.
```

Then, per project, `proxy.intercept: true` in configuration, followed by:

```
$ pkgreg mitm enable --confirm "pypi.org,files.pythonhosted.org,registry.npmjs.org"
$ systemctl restart pkgreg
```

The `--confirm` value must match the derived host list exactly. It exists so the list
cannot be widened by editing one file; the same string is what goes into the
certificate's name constraints, so policy and certificate cannot drift apart.

Startup then says so, every time:

```
INFO  interception ENABLED  hosts=pypi.org,files.pythonhosted.org,registry.npmjs.org
INFO  interception unknown-host policy=refuse
```

### 11.2 Developer, once per machine

```
$ ./pkgreg-client --persist -accept-interception \
    -server https://cache.example.com:8443 -project global \
    -ca-sha256 "AB:CD:..." -intercept-sha256 "9F:2C:...:44"
```

Without `-accept-interception` the client installs the ordinary CA and refuses the
second one. With it, before writing anything:

```
This installs a second certificate authority.

  It permits https://cache.example.com:8443 to present itself as:
      pypi.org
      files.pythonhosted.org
      registry.npmjs.org

  After this, tools on this machine cannot distinguish that cache from those
  hosts, and will not warn you. The cache can see the contents of requests to
  them. It cannot impersonate anything not listed above.

Continue? [y/N]
```

The listed hosts are read out of the certificate's own constraints, not from a claim
printed beside it — so the prompt cannot say one thing while the certificate permits
another.

Then, for Docker builds, the proxy settings from option A:

```
$ ./pkgreg-client -docker-build-trust
```

### 11.3 What a build looks like afterwards

This is the entire point of the option. Before — what the tutorial ships today:

```dockerfile
ARG BASE
FROM ${BASE}
ARG PKGREG
ARG PROJECT=global
ARG APT_PROXY
ARG PIP_INDEX_URL=${PKGREG}/${PROJECT}/pypi/root/pypi/+simple/
ARG UV_DEFAULT_INDEX=${PKGREG}/${PROJECT}/pypi/root/pypi/+simple/
ARG NPM_CONFIG_REGISTRY=${PKGREG}/${PROJECT}/npm/
ARG GIT_CONFIG_COUNT=1
ARG GIT_CONFIG_KEY_0=url.${PKGREG}/${PROJECT}/git/github.com/.insteadOf
ARG GIT_CONFIG_VALUE_0=https://github.com/
ARG http_proxy=${APT_PROXY}
ARG no_proxy=127.0.0.1,localhost
RUN pip install six
```

```
docker build --network=host \
  --build-arg BASE=... --build-arg PKGREG=... \
  --build-arg PROJECT=... --build-arg APT_PROXY=... -t myapp:dev .
```

After:

```dockerfile
FROM python:3.12-slim
RUN pip install six
RUN npm ci
RUN apt-get update && apt-get install -y curl
```

```
docker build .
```

Nothing in the file mentions pkgreg. Nothing on the command line does. The Dockerfile
is the one the developer would have written if the cache did not exist, and it is the
same file that works on a laptop with no cache configured at all.

### 11.4 CI

Identical, because it is the same two one-time installs baked into the runner image:
`--persist -accept-interception` and `-docker-build-trust`. Pipeline definitions need
no pkgreg-specific steps, and a job's `docker build .` is unmodified.

### 11.5 What this does **not** cover on its own

`FROM python:3.12-slim` is resolved by the **daemon or builder**, not by a `RUN` step,
so it does not use the build's proxy settings. Interception alone therefore does not
route base images through the cache. To get that as well, one of:

- **option E** (registry-mirror mode) — the clean answer; or
- proxy settings and interception trust on the daemon itself, via a systemd drop-in or
  `buildkitd.toml`, which is a separate installation and a separate decision.

The design should not pretend otherwise, and the tutorial must not imply that turning
this on makes image pulls cached too.

### 11.6 How anyone can tell it is on

Deliberately, because "the developer notices nothing" is both the feature and the risk:

- The console shows an interception badge on the project, listing the hosts.
- `pkgreg doctor` reports it in the posture section on both server and client.
- A developer can check for themselves, and the answer is legible:

```
$ openssl s_client -proxy cache.example.com:3142 -connect pypi.org:443 \
    </dev/null 2>/dev/null | openssl x509 -noout -issuer
issuer=CN = pkgreg interception CA
```

### 11.7 Turning it off

```
$ pkgreg mitm disable
```

Immediate, no client changes, no restart of anything on a developer's machine. Builds
that relied on it start reaching upstream directly again — they still work, they are
simply no longer cached, which is the correct direction for a failure.

Clients keep an unused, name-constrained CA until `--persist -uninstall`, which is
harmless but should still be cleaned up; `pkgreg doctor` on the client mentions it.

### 11.8 What breaks

- **Tools that pin certificates.** They will fail, correctly, and no configuration
  fixes that. `doctor` warns for upstreams known to pin.
- **Anything reaching a host not on the list**, under the default `refuse` policy — it
  gets a `403` naming the host, not a timeout. That message is the whole diagnostic.
- **A stale client trust store after a CA rotation**, which surfaces as certificate
  errors until `--persist` is re-run. The grace period in §4.6 exists to make this a
  scheduled task rather than an outage.

## 12. Implementation plan

Phased so that the useful, low-risk half ships first and the dangerous half is a
separate, revertible decision.

### Phase 1 — blind tunnel (option F-lite)
Small, no new trust, useful alone.
- `internal/app/dataplane.go:259`: replace the blanket CONNECT refusal with a policy
  switch (`refuse` default, `tunnel` opt-in).
- Reuse `Ctx.ProxyHostAllowed` for the tunnel allowlist.
- Byte relay with idle and total timeouts, connection caps, and no inspection.
- Tests: refused by default; tunnels only allowlisted hosts; never intercepts.

### Phase 2 — the constrained CA, no interception yet
- `internal/pki`: `LoadOrCreateInterceptionCA(dir, names)` producing the ECDSA,
  name-constrained, 1-year root. Explicit creation only.
- `pkgreg mitm-ca init|rotate|show`.
- Export-exclusion test; permissions test; a test asserting a certificate for a
  non-permitted name **fails verification against the root**, proving the constraint
  rather than asserting we wrote the field.

### Phase 3 — leaf minting
- `internal/pki/leaf.go`: mint, LRU, rate limit, per-host key. Memory only.
- Fuzz/property tests on host parsing; refusal of IPs, wildcards, non-permitted names.

### Phase 4 — the intercepting listener
- `internal/proxy/intercept.go`: CONNECT → policy → `tls.Server` with
  `GetCertificate` → inner `http.Server` → existing dispatch.
- SNI/host agreement checks; ALPN mirroring; upstream transport per host with system
  roots.
- The redactor from §9.1 applied at the boundary.

### Phase 5 — surfaces
- Config keys, `doctor` checks, console status, client `-accept-interception`, tutorial
  and this document's operator half.

## 13. Test plan (security-specific)

Beyond the functional tests, these must exist and must fail loudly:

1. A leaf for a name outside the constraints fails verification against the
   interception root. *(Proves §4.3 with the verifier, not with a struct field.)*
2. The ordinary CA cannot sign an interception leaf — `MaxPathLen: 0` still holds.
3. Interception is off with default configuration; a `CONNECT` gets 403.
4. `CONNECT` to a non-allowlisted host is never intercepted under any policy.
5. SNI ≠ CONNECT host is refused.
6. Upstream verification failure produces an error, and no cached content is served as
   fresh.
7. `InsecureSkipVerify` appears nowhere in the intercepting path — a source-level
   assertion test, since this one is a review failure away from being catastrophic.
8. `Authorization`/`Cookie` never appear in audit rows, job logs, or error strings, for
   an intercepted request that carried them.
9. A client credential is not forwarded upstream.
10. Air-gap export never contains `mitm-ca.key`.
11. Leaf mint rate limit holds under varied-SNI flood.
12. `mitm disable` stops interception without restarting clients.

## 14. Residual risks, accepted knowingly

- A compromised cache host can serve malicious packages **as the real upstream**, to
  every trusting client, for the allowlisted hosts. Name constraints bound *which*
  identities; they do not bound this one.
- Developers lose the ability to detect a substituted index by inspecting the
  certificate, because the certificate now legitimately says `pypi.org`. This is the
  cost of the feature working at all.
- Pinning tools break.
- Any future feature that wants to "look at" intercepted traffic will find the
  plumbing already there. §2.3 is the only thing standing in its way, and it is a
  sentence in a document.

## 15. Variant: interception in the client, not the server

**This variant is better than everything above, and it should be the default plan.**

### 15.1 The idea

The interception proxy runs inside `pkgreg-bridge`, on the developer's own machine,
instead of inside the cache. The bridge already terminates nothing and speaks plain HTTP
on loopback; it would additionally answer `CONNECT`, mint a leaf for the requested host
from a CA **generated on that machine**, terminate that TLS session locally, and
translate the request into the ordinary pkgreg request it already knows how to make —
forwarded to the cache over the same verified, fingerprint-pinned TLS as today.

The cache is unchanged. It never learns that interception is happening, never holds an
impersonation key, and never sees a request it would not have seen anyway.

### 15.2 Why the security calculus collapses

The whole of §1–§14 exists because a key that can impersonate `pypi.org` would live on a
shared server and be trusted by every machine in the organisation. Move that key to the
machine it serves and the argument evaporates:

| | Server-side (§0–§14) | Client-side |
|---|---|---|
| Where the impersonation key lives | shared cache host | the developer's own machine |
| Who trusts it | every client in the org | that one machine |
| Cache compromise ⇒ | impersonate upstreams to the whole fleet | **nothing new** — cache holds no such key |
| Key theft requires | remote compromise of one server | local code execution as that user |
| What key theft then buys the attacker | fleet-wide upstream impersonation | **≈ nothing** |

That last row is the crux. An attacker who can read a CA key sitting in a user's own
directory, held by a process running as that user, already has code execution as that
user — at which point they can modify the packages, the interpreter, the shell, or the
build script directly. The key grants no capability they did not already have. A
server-side key grants a capability nobody had.

It also removes the organisational decision entirely. Server-side interception needs
someone with authority over security posture to sign off, because it changes what a
shared service is. Client-side interception is a flag on a program a developer already
runs on their own machine. Adoption goes from "convince the security team" to "try it".

### 15.3 What is unchanged

- **Name constraints still apply** (§4.3), for the same reason: they bound the damage
  from the key regardless of where it sits, and they make the consent prompt truthful.
- **Certificate pinning still breaks** (§7.4).
- **Strict upstream verification** — here it is the bridge's existing pinned connection
  to the cache, which is already correct.
- **Credential handling gets easier**, not harder: the bridge translates an intercepted
  request into a normal pkgreg request, so a client's own upstream credentials are
  dropped at the point of translation rather than needing §9.2's discipline on a server.

### 15.4 The honest catches

1. **The build container must still trust the CA.** That is inherent to interception,
   not to where it runs, so client-side does not escape it. What changes is how much
   that matters: a *local* CA baked into a locally-built image is a certificate that
   grants power over one machine, and if it leaks with the image it is worthless to
   anyone else. A *server* CA baked into an image is a distributable impersonation
   capability. The objection that started this whole thread is much weaker here.

2. **Docker Desktop.** `internal/clientbridge/bridge.go` binds loopback only, with an
   explicit comment: the socket is unauthenticated and carries a project credential, so
   exposing it would hand that credential to anyone who can reach the port. A build on
   Docker Desktop lives in a VM that cannot reach the host's `127.0.0.1`. Reaching it
   via `host.docker.internal` requires binding wider, which requires authenticating the
   bridge socket **first**. That is a real prerequisite, not a detail — until it is
   done, this variant is Linux-with-`--network=host` only.

3. **It needs a stable address.** `clientbridge.Session` binds `127.0.0.1:0`, a fresh
   ephemeral port per session; `~/.docker/config.json` is a static file. So this is a
   `--persist`-shaped feature built on the fixed-port standalone bridge (41999), not a
   temporary-shell feature. That is a genuine shift for a client whose stated identity
   is "nothing persists, nothing to uninstall", and it should be named rather than
   glossed.

4. **Build-cache churn.** A per-session CA would invalidate the image layer carrying it
   on every run. The CA therefore wants to be per-machine and stable, which is slightly
   less ephemeral than the client's usual posture — the same tension as (3).

5. **N machines, N keys.** More keys exist than in the server-side design. Each is
   bounded to the machine it sits on, so the aggregate risk is still far lower, but the
   count is higher and there is no central revocation. "Turn it off" is per machine.

### 15.5 Where this design is weak

Written after the fact, against §15.2, which overstated its case.

**W1 — Name constraints depend on the client enforcing them, but only where the CA has
escaped the machine.** *(Revised: the first version of this overstated the risk.)*

A TLS stack that ignores name constraints on a trust anchor treats the CA as
unconstrained. Go and OpenSSL enforce them (verified); not every stack in every base
image does. But for an unconstrained key to be worth anything, an attacker needs three
things at once: the key, a client that trusts it, and a way to route that client's
traffic through them. In the purely local case they cannot have the key without already
having code execution as this user — at which point editing the Dockerfile or replacing
the interpreter is a shorter path to the same outcome. The marginal gain there really is
close to zero.

It survives in exactly two situations, and both are the CA failing to stay local:

- **Installed machine-wide (W6).** The browser then trusts it too, and user-level code
  execution plus an unconstrained CA becomes a quiet route to SSO credentials. Fixing
  W6 — install only where builds read it — removes this.
- **Escaped the machine (W2 shared image, W5 backup or sync).** Other people's machines
  now trust it, and their TLS stack's enforcement is the only bound. Nothing is local
  here and W1 applies in full.

So W1 is not an independent flaw; it is a multiplier on W2, W5 and W6. Fix those three
and its residual is negligible. This also reframes what name constraints are for: not
defence against someone who already owns your laptop, but containment for precisely the
cases where "it stays local" turns out to be false — which makes them more worth having,
not less.

**W2 — A shared image carries the trust anchor to machines that never consented.**
§15.4(1) claims a local CA baked into a local image is "worthless to anyone else if it
leaks". That is true of the key and false of the consequence. If the image is pushed to
a shared registry and used as a base by a colleague, that colleague's builds now trust a
CA whose private key sits on someone else's laptop — and can be impersonated by whoever
controls that laptop, for the constrained hosts. Trust anchors travel with images. Any
image carrying an interception CA must be marked unshippable, and ideally the CA should
be mounted per build rather than baked at all.

**W3 — The always-on bridge is a materially worse target than the session bridge.**
§15.4(3) treats "needs a stable address" as an ergonomic wrinkle. It is a security
regression. Today's bridge lives for one shell, on an ephemeral port, and dies on exit.
The variant needs a long-running process on a fixed, well-known port (41999) holding
both a project credential and a CA key, started at login. That widens the exposure
window from minutes to always, makes the port trivially discoverable, and gives an
attacker with brief code execution somewhere durable to persist.

**W4 — Loopback is not a boundary between users.** The bridge socket is unauthenticated
by design (`bridge.go:170`), so on a shared machine any local user can reach it and
spend the project credential it holds. True today; W3 makes it easier to find and
available for longer. Authenticating the socket is a prerequisite for this variant, not
a nice-to-have.

**W5 — "The key never leaves the machine" is not automatic.** A CA key placed under the
user's home directory is a CA key inside Time Machine, iCloud Drive, OneDrive and every
backup agent. The claim only holds if the storage path is deliberately chosen to be
excluded from sync and backup, and even then it is a convention, not a control.

**W6 — Installing into the host trust store is far broader than the build.** If the CA
lands in the OS trust store, the developer's *browser* also trusts a certificate
authority for `pypi.org`. Nothing in the build case needs that. The CA should go only
where builds read it, never machine-wide.

**W7 — No revocation, per machine.** There is no CRL, no OCSP, and no central list.
A stolen laptop leaves a valid interception CA with no mechanism to withdraw it, and W2
means copies may exist in images elsewhere. Short leaf and CA lifetimes are the only
real mitigation.

**W8 — Fail-open versus fail-closed is an unmade decision.** If the cache is
unreachable, does the bridge tunnel to the real upstream (builds keep working, silently
uncached, and an attacker who can DoS the cache can force direct egress) or fail
(builds break)? Both are defensible; picking neither is not.

**W9 — §15.2's table can be misread.** "Cache compromise ⇒ nothing new" is about
*impersonation only*. A compromised cache can still serve a malicious package to every
client, exactly as §14 says. Client-side interception does not improve that at all.

**W10 — The consent prompt is information, not a control.** §11.2 reads the permitted
hosts out of the certificate so the prompt cannot lie. If an attacker has already
replaced the local CA, the prompt faithfully prints *their* certificate's names. It
helps an honest user understand; it stops nothing.

None of W1–W10 is fatal, and every one of them is smaller than the corresponding
server-side problem. W2, W3 and W4 are the ones that need answers in the design rather
than in documentation.

### 15.6 What it would take to build

Materially less than the server-side design, and it touches no server security surface:

- `internal/pki`: the constrained-CA and leaf-minting work from Phases 2–3 — unchanged,
  but linked into the client binary instead of the server.
- `internal/clientbridge`: answer `CONNECT`, `tls.Server` with `GetCertificate`, map
  intercepted host → existing bridge route. The proxying, rewriting and pinned upstream
  connection all already exist.
- A `-intercept` flag with the same consent prompt as §11.2, listing hosts read from the
  certificate's constraints.
- Prerequisite for Docker Desktop only: authenticate the bridge socket, then allow a
  non-loopback bind behind an explicit flag.

No server change. No operator ceremony. No org-wide sign-off. Sections §4.3, §5, §7.4
and §9 carry over intact; §2, §4.1, §4.4–4.6, §6.2, §10 and §11.1 mostly cease to apply.

## 16. Recommendation

1. **Ship Phase 1 (blind CONNECT tunnelling) regardless.** A few days, no new trust,
   trivially revertible, and it fixes a real defect: today setting `HTTPS_PROXY`
   anywhere breaks every https fetch in a build, when it should quietly pass through.

2. **Then build §15, client-side interception** — if interception is wanted at all.
   It delivers the same unchanged-Dockerfile outcome, needs no server change, and the
   impersonation key never leaves the machine it serves. Its blockers are ordinary
   engineering (authenticate the bridge socket, stable address) rather than a change in
   what a shared service is permitted to do.

3. **Do not build server-side interception (§0–§14)** unless something specific requires
   it that §15 cannot provide — for example a fleet where developers cannot run a local
   process at all, or CI runners that must be configured centrally with no per-runner
   install. If that requirement appears, §0–§14 is the design to follow, and it should
   not begin until someone with authority over the organisation's security posture has
   read §1, §2 and §14 and said yes in writing.

The engineering in all three is tractable. Only (3) asks a question that is not an
engineering question, which is precisely why (2) is the better answer to the same need.
