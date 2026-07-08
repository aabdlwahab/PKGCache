"""Client-facing and internal URL derivation, per ecosystem and per project.

Everything HTTPS lives on the ONE unified port (see pkgcache/unified.py): docker at
/v2/… with the project riding the image name, and npm/pypi/git/files at the fully
qualified /<project>/<role>/… (the default project is the literal `global`). apt/apk
is the exception — a plain-HTTP forward proxy on its own port, with the project as
the proxy username. This module turns (project, eco) into the three URL shapes the
backend needs: the client-facing pull endpoints (with copy-paste setup instructions)
shown in the UI, the internal `pkgcache`-container progress feeds the live poller
hits, and the /healthz probes. Ports and prefix rules come from the projects
service, so nothing is hard-coded here."""
from app.services import projects

# eco label → pkgcache role, and the scheme each role speaks (apt is the plain-HTTP
# proxy port; everything else is the unified TLS port).
_ECO_ROLE = {eco: role for role, eco in projects.ROLE_SUBDIR.items()}  # docker→oci, pip→pypi, …
_ECO_SCHEME = {eco: ("http" if role == "apt" else "https") for eco, role in _ECO_ROLE.items()}

# The progress path each role's sub-app serves (relative to its admin prefix).
_PROGRESS_PATH = {"docker": "/v2/_progress", "npm": "/-/progress",
                  "pip": "/+progress", "apt": "/acng-progress", "git": "/+progress",
                  "files": "/+progress"}


def _admin_base(project, eco):
    """scheme://pkgcache:<port>/<project>/<role> — the uniform internal admin base
    every role answers on (the routers strip the prefix; see projects.role_prefix)."""
    role = _ECO_ROLE[eco]
    return (f"{_ECO_SCHEME[eco]}://pkgcache:{projects.ROLE_PORT[role]}"
            f"{projects.role_prefix(project, role)}")


def progress_sources(project=projects.GLOBAL):
    """{eco: progress URL} on the `pkgcache` container for THIS project. Per-project
    progress registries live behind the shared ports, reached by the admin prefix."""
    return {eco: _admin_base(project, eco) + rel for eco, rel in _PROGRESS_PATH.items()}


def health_sources(project=projects.GLOBAL):
    """{eco: /healthz URL} on the `pkgcache` container. Health is per (project, role)
    sub-app, but all projects share one server per role — probing global tells you
    whether the role is up for everyone, so the poller only probes the given project."""
    return {eco: _admin_base(project, eco) + "/healthz" for eco in _PROGRESS_PATH}


def pypi_internal(project=projects.GLOBAL):
    """(internal base URL on the `pkgcache` container, public path prefix) for a
    project's pypi role. The lock warmer drives the internal base to pull each locked
    file into the cache; the public prefix is what the rewritten lock's URLs carry
    (always fully qualified: `/global/pypi` or `/<project>/pypi`)."""
    prefix = projects.role_prefix(project, "pypi")
    return f"https://pkgcache:{projects.ROLE_PORT['pypi']}{prefix}", prefix


def endpoints(project=projects.GLOBAL):
    """Client-facing pull endpoints per ecosystem, as DATA the console renders:

        {eco: {"url": <the base URL to copy>,
               "note": <one-line hint>,
               "setup": [<copy-paste command lines to go from zero to pulling>]}}

    `<host>` is a literal placeholder the operator substitutes (the console swaps in
    its own hostname where it can). Every project INCLUDING global gets the same
    fully qualified shapes; only docker (image-name prefix, none for global) and
    apt/apk (proxy username, none for global) differ between global and named."""
    p = project or projects.GLOBAL
    uni = projects.UNIFIED_PORT
    apt = projects.APT_PORT
    img = "" if p == projects.GLOBAL else f"{p}/"          # docker image-name prefix
    at = "" if p == projects.GLOBAL else f"{p}@"           # apt proxy username
    ca = "/path/to/ca.crt"

    return {
        "docker": {
            "url": f"<host>:{uni}/{img}dockerhub/<image>",
            "note": "upstreams: dockerhub | ghcr | quay; Docker Hub official images live under library/",
            "setup": [
                f"# trust the cache's CA for this registry (one-time per host):",
                f"sudo mkdir -p /etc/docker/certs.d/<host>:{uni}",
                f"sudo cp ca.crt /etc/docker/certs.d/<host>:{uni}/ca.crt",
                f"# then pull through the cache:",
                f"docker pull <host>:{uni}/{img}dockerhub/library/alpine:3.20",
                f"docker pull <host>:{uni}/{img}ghcr/<org>/<image>:<tag>",
            ],
        },
        "npm": {
            "url": f"https://<host>:{uni}/{p}/npm/",
            "note": "set once with npm config, or per-install with --registry",
            "setup": [
                f"npm config set registry https://<host>:{uni}/{p}/npm/",
                f"npm config set cafile {ca}",
                "npm install <pkg>",
            ],
        },
        "pip": {
            "url": f"https://<host>:{uni}/{p}/pypi/root/pypi/+simple/",
            "note": "other indexes: root/pytorch-cu124, root/pytorch-cpu, … (see /+indexes)",
            "setup": [
                f"pip install --index-url https://<host>:{uni}/{p}/pypi/root/pypi/+simple/ --cert {ca} <pkg>",
                f"# uv:",
                f"UV_INDEX_URL=https://<host>:{uni}/{p}/pypi/root/pypi/+simple/ SSL_CERT_FILE={ca} uv pip install <pkg>",
                f"# or persist in ~/.config/pip/pip.conf:  index-url = … / cert = {ca}",
            ],
        },
        "apt": {
            "url": f"http://{at}<host>:{apt}/",
            "note": "plain-HTTP forward proxy; keep http:// mirror lines"
                    + ("" if p == projects.GLOBAL else f"; the '{p}@' username selects this project"),
            "setup": [
                f"echo 'Acquire::http::Proxy \"http://{at}<host>:{apt}\";' | sudo tee /etc/apt/apt.conf.d/01proxy",
                "sudo apt-get update && sudo apt-get install -y <pkg>",
            ],
        },
        "apk": {
            "url": f"http://{at}<host>:{apt}/",
            "note": "same proxy as apt; switch /etc/apk/repositories to http:// first",
            "setup": [
                "sed -i 's/https/http/' /etc/apk/repositories",
                f"http_proxy=http://{at}<host>:{apt} apk add --no-cache <pkg>",
            ],
        },
        "git": {
            "url": f"https://<host>:{uni}/{p}/git/<upstream-host>/<owner>/<repo>.git",
            "note": "read-only mirror-and-serve; the real upstream host goes in the path",
            "setup": [
                f"git config --global http.\"https://<host>:{uni}/\".sslCAInfo {ca}",
                f"# transparent adoption (covers submodules, pip git+https, CPM, …):",
                f"git config --global url.\"https://<host>:{uni}/{p}/git/github.com/\".insteadOf \"https://github.com/\"",
                f"# or clone explicitly:",
                f"git clone https://<host>:{uni}/{p}/git/github.com/<owner>/<repo>.git",
            ],
        },
        "files": {
            "url": f"https://<host>:{uni}/{p}/files/<path>",
            "note": "generic artifacts: anonymous GET; PUT/DELETE need this project's write token",
            "setup": [
                f"wget --ca-certificate=ca.crt https://<host>:{uni}/{p}/files/<path>",
                f"# upload (token from the console's Artifacts panel):",
                f"curl --cacert ca.crt -T <file> -H \"Authorization: Bearer $TOKEN\" \\",
                f"     https://<host>:{uni}/{p}/files/<path>",
            ],
        },
    }
