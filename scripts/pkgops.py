#!/usr/bin/env python3
"""Operator CLI for the air-gap cache operations — checkpoint, shuttle
export/import, rollback, mode. Run it by hand on either side of the gap:

    python3 scripts/pkgops.py checkpoint "added numpy 2.1 + torch 2.3"
    python3 scripts/pkgops.py export /media/shuttle
    python3 scripts/pkgops.py export /media/shuttle --base <sha> --target <sha>
    python3 scripts/pkgops.py import /media/shuttle
    python3 scripts/pkgops.py rollback <commit>
    python3 scripts/pkgops.py mode offline

This is a thin wrapper: the actual logic lives in the backend module
webui/ops.py (the control UI imports the very same code), so the CLI and the UI
can never drift. Here we just parse args, stream the op's log lines to stdout,
and exit non-zero on failure.
"""
import argparse
import pathlib
import sys

# The canonical implementation is the backend's ops module; import it from there
# so there is a single source of truth.
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent / "webui"))
import ops  # noqa: E402


def main(argv=None):
    parser = argparse.ArgumentParser(prog="pkgops", description=__doc__.splitlines()[0])
    # Which project's cache to act on (default: the global cache with the original
    # URLs). A named project has its own cache tree, git+DVC repo and shuttle:
    #   python3 scripts/pkgops.py --project projA checkpoint "added torch"
    parser.add_argument("--project", default="global",
                        help="project to operate on (default: global)")
    sub = parser.add_subparsers(dest="action", required=True)

    p = sub.add_parser("checkpoint", help="hash → commit the cache (live, no downtime)")
    p.add_argument("message")

    # Export/import use FIXED dirs (shuttle/out, shuttle/in) — the operator copies
    # them to/from the removable media by hand. No drive argument.
    p = sub.add_parser("export", help="stage the cache into shuttle/out (online side)")
    p.add_argument("--base", help="base checkpoint (omit for a FULL export — a fresh machine)")
    p.add_argument("--target", help="target checkpoint (with --base, exports that delta)")

    sub.add_parser("import", help="apply the shuttle in shuttle/in (air-gapped side)")

    p = sub.add_parser("rollback", help="restore the cache to a checkpoint")
    p.add_argument("commit")

    p = sub.add_parser("mode", help="switch the cache online/offline")
    p.add_argument("target", choices=["online", "offline"])

    args = vars(parser.parse_args(argv))
    action = args.pop("action")
    try:
        for line in ops.Operations().build(action, args):
            sys.stdout.write(line)
            sys.stdout.flush()
    except ops.OpError as exc:
        sys.stderr.write(f"\n[error] {exc}\n")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
