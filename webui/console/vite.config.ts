import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Dev: the SPA runs on :5173 and proxies /api to the Python webui (default :8088)
// so there is no CORS and the same paths work in prod (nginx proxies /api too).
// Override the dev backend with PKGCACHE_WEBUI=http://host:port.
const API_TARGET = process.env.PKGCACHE_WEBUI || "http://127.0.0.1:8088";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target: API_TARGET, changeOrigin: true },
    },
  },
  build: {
    outDir: "dist",
    sourcemap: false,
    chunkSizeWarningLimit: 900,
  },
});
