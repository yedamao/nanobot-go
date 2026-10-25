import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The gateway embeds the build (internal/webui, //go:embed static) and
// serves it under /ui/, so the bundle is written straight there and
// committed: Go builds and tests need no Node.
//
// `npm run dev` serves the app on http://localhost:5173/ui/ and proxies
// the gateway's endpoints to a gateway running on 127.0.0.1:18790. The
// Host header stays localhost, so the gateway's loopback and same-origin
// checks pass.
const gateway = "http://127.0.0.1:18790";

export default defineConfig({
  base: "/ui/",
  plugins: [react()],
  build: {
    outDir: "../internal/webui/static",
    emptyOutDir: true,
    // Never inline assets as data: URLs; the CSP allows only 'self'.
    assetsInlineLimit: 0,
  },
  server: {
    proxy: {
      "/ws": { target: gateway, ws: true },
      "/ui/chats": gateway,
      "/ui/status": gateway,
      "/ui/settings": gateway,
    },
  },
});
