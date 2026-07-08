#!/usr/bin/env python3
"""Pre-seed the caches before going air-gapped.

Reads a seed file (see pkgcache/seed.example.yaml) and drives the *canonical client
fetch* for each entry THROUGH the local online proxies — so the cache and the
SQLite ledger populate exactly as a real install would, reusing the real protocol
code. Run this on the online side before `pkgops.py checkpoint`.

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
from concurrent.futures import ThreadPoolExecutor

import yaml

HOST = os.environ.get("CACHE_HOST", "localhost")
CA = os.environ.get("CA_CERT", "certs/ca.crt")
# The unified HTTPS port (docker + npm/pypi/git/files) and the apt/apk proxy port.
PORT = os.environ.get("CACHE_PORT", "8443")
APT_PORT = os.environ.get("CACHE_APT_PORT", "3142")
# Which project's cache to warm; "global" is the default project.
PROJECT = os.environ.get("CACHE_PROJECT", "global")
# Docker carries the project in the image name; global images are unprefixed.
_IMG = "" if PROJECT == "global" else f"{PROJECT}/"
# apt/apk carry the project as the proxy username; global has none.
_AT = "" if PROJECT == "global" else f"{PROJECT}@"
# Images are pulled concurrently across the seed list (each `docker pull` already
# parallelises its own layers). Bounded because Docker Hub anonymous pulls are
# rate-limited; shared base layers across images coalesce in the proxy's inflight
# registry, so overlap never duplicates an upstream download.
DOCKER_JOBS = int(os.environ.get("DOCKER_JOBS", "4"))


def run(cmd: list[str], **kwargs) -> bool:
    print("==>", " ".join(shlex.quote(c) for c in cmd))
    return subprocess.run(cmd, **kwargs).returncode == 0


def do_docker(refs: list[str]) -> None:
    if not refs:
        return
    with ThreadPoolExecutor(max_workers=min(DOCKER_JOBS, len(refs))) as pool:
        list(pool.map(lambda ref: run(["docker", "pull", f"{HOST}:{PORT}/{_IMG}{ref}"]), refs))


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
                "--index-url", f"https://{HOST}:{PORT}/{PROJECT}/pypi/{index}/+simple/", req,
            ])


def do_npm(pkgs: list[str]) -> None:
    with tempfile.TemporaryDirectory() as d:
        for pkg in pkgs:
            run(["npm", "pack", pkg, "--registry", f"https://{HOST}:{PORT}/{PROJECT}/npm/",
                 "--cafile", CA], cwd=d)


def do_apt(pkgs: list[str]) -> None:
    if not pkgs:
        return
    script = (
        f'echo "Acquire::http::Proxy \\"http://{_AT}{HOST}:{APT_PORT}\\";" > /etc/apt/apt.conf.d/01proxy && '
        "apt-get update && apt-get install -d -y " + " ".join(map(shlex.quote, pkgs))
    )
    run(["docker", "run", "--rm", "--add-host", f"{HOST}:host-gateway",
         "ubuntu:24.04", "sh", "-c", script])


def do_apk(pkgs: list[str]) -> None:
    if not pkgs:
        return
    script = f"http_proxy=http://{_AT}{HOST}:{APT_PORT} apk fetch --no-cache " + " ".join(map(shlex.quote, pkgs))
    run(["docker", "run", "--rm", "--add-host", f"{HOST}:host-gateway",
         "alpine:3.20", "sh", "-c", script])


def do_git(repos: list[str]) -> None:
    """Warm git mirrors by hitting info/refs — `git ls-remote` through the cache
    triggers the server-side clone --mirror. Entries are `<upstream-host>/<owner>/
    <repo>`, e.g. github.com/octocat/Hello-World."""
    env = dict(os.environ, GIT_SSL_CAINFO=CA)
    for repo in repos:
        run(["git", "ls-remote", f"https://{HOST}:{PORT}/{PROJECT}/git/{repo}.git"], env=env)


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
    do_git(seed.get("git", []) or [])
    print("==> prefetch complete; run `pkgops.py checkpoint` to version the delta.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
