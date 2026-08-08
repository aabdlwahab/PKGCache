# Self-hosted fonts (air-gap)

Drop the IBM Plex Mono `woff2` file here so the console renders pixel-accurately
offline (no CDN). The CSS in `src/styles/tokens.css` references:

- `/fonts/IBMPlexMono.woff2`

(The UI is monospace throughout, so only IBM Plex Mono is needed.)

If these files are absent the app still works — it falls back to the system
monospace / sans stacks declared alongside the `@font-face` rules.

Get them from the IBM Plex release (https://github.com/IBM/plex) on an online
machine, place the woff2 here, then build. Filenames must match the URLs above
(or edit the `src:` lines in tokens.css to match what you ship).
