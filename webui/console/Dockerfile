# Multi-stage build for the React console: Node builds the static bundle, then a
# tiny nginx image serves it and reverse-proxies /api to the Python webui. No Node
# is needed at runtime or on the host — only at build time (on the online side).
#
#   docker compose --profile online --profile ui up -d --build console
#
# Build the image on the online side; the air-gapped side just pulls it out of the
# docker cache like any other image.

# ---- build stage --------------------------------------------------------------
FROM node:20-alpine AS build
WORKDIR /app
COPY package.json ./
# No committed lockfile, so use install (not ci). Pin happens via package.json ranges.
RUN npm install --no-audit --no-fund
COPY tsconfig.json vite.config.ts index.html ./
COPY src ./src
COPY public ./public
RUN npm run build

# ---- serve stage --------------------------------------------------------------
FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1/ >/dev/null 2>&1 || exit 1
