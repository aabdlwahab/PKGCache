"""Backend error types shared across layers.

OpError lives here (rather than in the operations service) so the proc gateway can
raise it on a failed subprocess without importing a service — keeping the gateway
free of upward dependencies. The HTTP layer turns OpError (a RuntimeError) into a
400. Phase 2 will add an ApiError(status) base and fold ProjectError in too."""


class OpError(RuntimeError):
    """A bad request (failed validation) or a failed step in a cache operation.
    Subclasses RuntimeError so the HTTP POST handler renders it as a 400."""
