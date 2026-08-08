# Go dependencies

Dependencies are kept deliberately small. Each direct dependency exists because the
standard library does not provide the required primitive.

| Module | Reason |
|---|---|
| `github.com/prometheus/client_golang` | Prometheus collectors and exposition format |
| `golang.org/x/sync` | Shared concurrency primitives used by the cache engine |
| `gopkg.in/yaml.v3` | Strict YAML configuration decoding |
| `modernc.org/sqlite` | Pure-Go SQLite driver, preserving static `CGO_ENABLED=0` builds |
