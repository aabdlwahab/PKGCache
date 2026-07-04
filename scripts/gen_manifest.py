#!/usr/bin/env python3
"""Export the cross-ecosystem manifest from the proxies' native SQLite ledgers.

This is a thin wrapper: the implementation (and the canonical eco→(subdir,
ecosystem) map) lives in the backend package at webui/app/manifest.py, so there is
ONE definition shared by the control UI and this CLI. The checkpoint op invokes this
script as a subprocess with PKGCACHE_MANIFEST_ROOT pointing at the project's repo.

    gen_manifest.py            # export manifests/*.json from the ledgers
    gen_manifest.py --rebuild  # first repopulate each ledger from disk (repair),
                               #   then export. Needs the pkgcache package importable.
"""
import pathlib
import sys

# Put webui/ on the path so the `app` package is importable, then defer to it.
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent / "webui"))
from app.manifest import main  # noqa: E402

if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
