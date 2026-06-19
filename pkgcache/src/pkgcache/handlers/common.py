"""Helpers shared by the protocol handlers."""
from __future__ import annotations

import re

from starlette.requests import Request

_WHEEL_RE = re.compile(r"^(?P<name>.+?)-(?P<ver>[^-]+)-.*\.whl$")
_SDIST_SUFFIXES = (".tar.gz", ".tar.bz2", ".tgz", ".zip")


def external_base(request: Request) -> str:
    """Absolute scheme://host the client reached us on, honoring the TLS proxy.

    Caddy terminates HTTPS and forwards plain HTTP, setting X-Forwarded-* (and
    X-outside-url for the pip block). URL rewriting must use these so links point
    back at the public https endpoint, not the internal container.
    """
    xou = request.headers.get("x-outside-url")
    if xou:
        return xou.rstrip("/")
    proto = request.headers.get("x-forwarded-proto") or request.url.scheme
    host = (
        request.headers.get("x-forwarded-host")
        or request.headers.get("host")
        or request.url.netloc
    )
    return f"{proto}://{host}"


def normalize_pypi_name(name: str) -> str:
    """PEP 503 normalization."""
    return re.sub(r"[-_.]+", "-", name).lower()


def parse_dist_filename(fn: str) -> tuple[str | None, str | None, str | None]:
    """(name, version, arch_tag) from a wheel/sdist filename, else (None, None, None)."""
    m = _WHEEL_RE.match(fn)
    if m:
        # platform/abi tag tail, for the ledger 'arch' column
        tag = fn[: -len(".whl")].split("-", 2)[-1] if fn.count("-") >= 2 else None
        return normalize_pypi_name(m.group("name")), m.group("ver"), tag
    for suf in _SDIST_SUFFIXES:
        if fn.endswith(suf):
            stem = fn[: -len(suf)]
            if "-" in stem:
                name, ver = stem.rsplit("-", 1)
                return normalize_pypi_name(name), ver, None
            return normalize_pypi_name(stem), None, None
    return None, None, None
