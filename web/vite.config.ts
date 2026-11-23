import { defineConfig } from "vite";
import vue from "@vitejs/plugin-vue";

// In development the dashboard runs on :5173 and proxies to the services
// started by docker compose, the same way nginx does in production. The
// browser only ever talks to one origin, so no CORS setup is needed.
export default defineConfig({
  plugins: [vue()],
  server: {
    proxy: {
      "/v1": { target: "http://localhost:8080", ws: true },
      "/worker": { target: "http://localhost:9091", rewrite: (p) => p.replace(/^\/worker/, "") },
      "/mock": { target: "http://localhost:8090", rewrite: (p) => p.replace(/^\/mock/, "") },
    },
  },
});
