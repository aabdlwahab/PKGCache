#!/usr/bin/env python3
"""Entry shim for the control UI.

The backend now lives in the `app` package (see app.main). This file stays at
webui/server.py so the Dockerfile CMD (`python3 webui/server.py`) and the operator's
muscle memory keep working. Running the script puts webui/ on sys.path[0], which is
what makes `app` importable without any path juggling.

  python3 webui/server.py            # then open http://127.0.0.1:8088
"""
from app.main import main

if __name__ == "__main__":
    main()
