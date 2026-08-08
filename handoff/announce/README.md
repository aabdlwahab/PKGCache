# pkgreg announcement handoff

This folder contains the paste-ready Teams announcement and its four animated
architecture cards. The copy and visuals describe the current Go implementation, not
the retired Python/nginx/container/DVC deployment.

## Files

- `TEAMS-POST.md` — short launch post plus the complete operator/feature appendix.
- `00-pkgcache-launch.gif` — Go release and single-binary overview. The historical
  filename is retained so existing attachment references do not break; the artwork
  itself says `pkgreg`.
- `01-one-cache.gif` — six clients, one shared engine and verified temporary session.
- `02-versioned.gif` — native checkpoints, delta export and bounded storage.
- `03-offline.gif` — peer-before-origin retrieval and verified air-gap transfer.
- `generate-visuals.go` — deterministic source for all four GIFs.

## Rebuild the GIFs

The generator uses the Go standard library and the system `rsvg-convert` command. It
writes through a temporary file and atomically replaces each GIF.

```bash
go run ./handoff/announce/generate-visuals.go
```

The output stays at 634 × 557 pixels so it can replace the earlier Teams assets
without changing the post layout.
