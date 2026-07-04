"""Domain services: the model layer. Each service owns one slice of behaviour
(projects, cache operations, jobs, live feed, reads, usage, lockwarm) and speaks
only to gateways and other services — never to HTTP request/response types."""
