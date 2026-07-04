"""The controller layer: the stdlib HTTP handler that parses requests, dispatches
to services, and serializes JSON. No git/dvc/sqlite/socket work lives here — it
delegates every side effect to a service or gateway."""
