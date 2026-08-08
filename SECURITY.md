# Security policy

> **Before this repository is made externally visible, confirm the reporting address
> below actually exists and is monitored.** It is derived from the project's domain,
> not from a verified alias. A security policy that routes reports into a black hole
> is worse than none.

## Reporting a vulnerability

Report suspected vulnerabilities privately to **security@brightskiesinc.com**.

Please do not open a public issue for a suspected vulnerability, and please do not
disclose it publicly until a fix is available.

Include whatever you have:

- the affected component (`pkgreg`, `pkgreg-client`, `pkgreg-bridge`, the console)
- the version — `pkgreg version` prints build identity, commit, and Go version
- how the instance is configured, in particular whether control-plane
  authentication is enabled, whether the listener is loopback-only, and whether
  `server.proxy_allowlist` is set
- steps to reproduce, and the impact you believe it has

### What to expect

| Stage | Target |
| --- | --- |
| Acknowledgement of your report | 3 working days |
| Initial assessment and severity | 10 working days |
| Fix or documented mitigation for a confirmed high/critical issue | 30 days |

We will tell you when we have reproduced the issue, when a fix lands, and when it
ships. If we disagree that a report is a vulnerability, we will say so and explain
why rather than going quiet.

## Supported versions

pkgreg has not yet reached 1.0. Until it does, only the current `main` branch
receives security fixes; there are no maintained release branches and no backports.
Once tagged releases exist this table will name them explicitly.

| Version | Supported |
| --- | --- |
| `main` | Yes |
| Untagged builds predating the current `main` | No — rebuild from `main` |

## Deployment posture matters to triage

Several behaviours are configuration-dependent by design, and knowing your
configuration changes whether a report is a vulnerability or an expected
consequence of a documented setting. Two in particular:

- **`server.proxy_allowlist`.** When empty, the apt/apk forward proxy relays
  plaintext HTTP to any upstream host. This is a deliberate, documented setting
  and is *not* treated as a vulnerability on its own — but the default being
  permissive is a known weakness we intend to change. `pkgreg doctor` fails when
  an empty allowlist is combined with a non-loopback listener. If you find a way
  to bypass a *configured, non-empty* allowlist, that is a vulnerability; report it.

- **Control-plane authentication.** `pkgreg init` provisions a superuser and
  enables enforcement. An operator who explicitly removes every account and unsets
  `auth.root_user` has an unauthenticated control plane by choice; `doctor` fails on
  that combination off loopback. A way to bypass authentication that *is* enabled is
  a vulnerability; report it.

If you are unsure which side of that line your finding falls on, report it and let
us make the call.

## Scope

In scope: the `pkgreg` server and its control plane, `pkgreg-client`,
`pkgreg-bridge`, the embedded console, the PKI and trust-bootstrap flow, air-gap
pack verification, and the released build and signing pipeline.

Out of scope: vulnerabilities in upstream package registries pkgreg caches from,
findings that require pre-existing root on the cache host, and denial of service
achieved purely by exhausting configured resource limits.

## Hardening a deployment

`pkgreg doctor` is the authoritative posture check — it fails, not warns, on unsafe
combinations and exits with a distinct status per class of problem. Run it before
exposing an instance to a network you do not control.
