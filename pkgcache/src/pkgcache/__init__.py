"""pkgcache — one Python codebase for four pull-through package caches.

Roles (PKGCACHE_ROLE): oci | npm | pypi | apt. Each role serves one ecosystem's
protocol over its own port, built on the shared `core` primitives.
"""

__all__ = ["__version__"]
__version__ = "0.1.0"
