"""Gateways: the only place the backend touches the outside world — subprocesses
(git/dvc/docker via proc), the per-ecosystem SQLite ledgers (ledgers), and the
pkgcache container's HTTP surface (pkgcache). Services depend on these; nothing
here depends back on a service except for shared constants."""
