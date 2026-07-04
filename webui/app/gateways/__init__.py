"""Gateways: the only place the backend touches the outside world — subprocesses
(git/dvc/docker via proc), the shared registry file (registry), and the pkgcache
container's HTTP surface (pkgcache), including its ledger admin endpoints. Services
depend on these; nothing here depends back on a service except for shared constants."""
