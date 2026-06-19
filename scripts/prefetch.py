#!/usr/bin/env python3
"""Pre-seed the caches before going air-gapped.

Reads a seed file (see pkgcache/seed.example.yaml) and drives the *canonical client
fetch* for each entry THROUGH the local online proxies — so the cache and the
SQLite ledger populate exactly as a real install would, reusing the real protocol
code. Run this on the online side before scripts/checkpoint.sh.

    CACHE_HOST=cache.local ./scripts/prefetch.py seed.yaml

Requirements on the host: docker (for images + apt/apk), pip, npm. apt/apk entries
are fetched inside throwaway ubuntu/alpine containers pointed at the proxy.
"""
from __future__ import annotations

import os
import shlex
import subprocess
import sys
import tempfile

import yaml

HOST = os.environ.get("CACHE_HOST", "localhost")
CA = os.environ.get("CA_CERT", "certs/ca.crt")


def run(cmd: list[str], **kwargs) -> bool:
    print("==>", " ".join(shlex.quote(c) for c in cmd))
    return subprocess.run(cmd, **kwargs).returncode == 0


def do_docker(refs: list[str]) -> None:
    for ref in refs:
        run(["docker", "pull", f"{HOST}:5000/{ref}"])


def do_pip(specs: list[str]) -> None:
    with tempfile.TemporaryDirectory() as d:
        for spec in specs:
            index, _, req = spec.partition(" ")
            if not req:
                print(f"  skip malformed pip entry: {spec!r}", file=sys.stderr)
                continue
            run([
                "pip", "download", "--no-deps", "--disable-pip-version-check",
                "--no-cache-dir", "--cert", CA, "--dest", d,
                "--index-url", f"https://{HOST}:3141/{index}/+simple/", req,
            ])


def do_npm(pkgs: list[str]) -> None:
    with tempfile.TemporaryDirectory() as d:
        for pkg in pkgs:
            run(["npm", "pack", pkg, "--registry", f"https://{HOST}:4873/",
                 "--cafile", CA], cwd=d)


def do_apt(pkgs: list[str]) -> None:
    if not pkgs:
        return
    script = (
        f'echo "Acquire::http::Proxy \\"http://{HOST}:3142\\";" > /etc/apt/apt.conf.d/01proxy && '
        "apt-get update && apt-get install -d -y " + " ".join(map(shlex.quote, pkgs))
    )
    run(["docker", "run", "--rm", "--add-host", f"{HOST}:host-gateway",
         "ubuntu:24.04", "sh", "-c", script])


def do_apk(pkgs: list[str]) -> None:
    if not pkgs:
        return
    script = f"http_proxy=http://{HOST}:3142 apk fetch --no-cache " + " ".join(map(shlex.quote, pkgs))
    run(["docker", "run", "--rm", "--add-host", f"{HOST}:host-gateway",
         "alpine:3.20", "sh", "-c", script])


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print("usage: prefetch.py <seed.yaml>   (CACHE_HOST, CA_CERT env)", file=sys.stderr)
        return 2
    seed = yaml.safe_load(open(argv[1])) or {}
    do_docker(seed.get("docker", []) or [])
    do_pip(seed.get("pip", []) or [])
    do_npm(seed.get("npm", []) or [])
    do_apt(seed.get("apt", []) or [])
    do_apk(seed.get("apk", []) or [])
    print("==> prefetch complete; run scripts/checkpoint.sh to version the delta.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
