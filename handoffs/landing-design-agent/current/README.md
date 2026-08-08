# Previewing and integrating the current landing page

## Preview

```bash
cd handoffs/landing-design-agent/current
python3 -m http.server 4173
```

Open `http://127.0.0.1:4173/landing.html`.

The font file is not included in this handoff. The CSS first tries a locally
installed IBM Plex Mono and otherwise uses the system monospace fallback.

## Production source locations

The canonical implementation lives at:

- `webui/console/public/landing.html`
- `webui/console/public/landing.css`
- `webui/console/public/landing.js`
- `webui/console/public/theme.js`

The files in this directory are snapshots for design work. Apply final approved
changes to the canonical files, then refresh this package.

## Runtime constraints

- Static HTML, CSS, and JavaScript.
- No framework dependency on the marketing page.
- Strict self-hosted CSP compatibility.
- `/` serves the landing page in nginx; `/console` serves the React application.
- Desktop, tablet, mobile, reduced-motion, and no-JavaScript states must remain
  usable.

