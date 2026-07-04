"""Backend error types shared across layers.

ApiError is the one contract between services and the controller: anything a service
raises to signal a bad request or a failed step carries the HTTP status it should
map to, and the dispatcher renders it as {"error": message} with that status.
Anything that is NOT an ApiError is a genuine bug and propagates to a 500 — so a
stray ValueError no longer masquerades as a client 400.

ApiError lives here (not on a service) so the proc gateway can raise OpError on a
failed subprocess, and the projects service can raise ProjectError, without either
importing the other or the HTTP layer."""


class ApiError(Exception):
    """A service-level error the controller maps to an HTTP status. Default 400
    (bad request); pass a different status for not-found / conflict / upstream."""

    status = 400

    def __init__(self, message, status=None):
        super().__init__(message)
        self.message = str(message)
        if status is not None:
            self.status = status


class OpError(ApiError):
    """A bad request (failed validation) or a failed step in a cache operation.
    Maps to 400. The operator CLI (scripts/pkgops.py) also catches this by type."""
