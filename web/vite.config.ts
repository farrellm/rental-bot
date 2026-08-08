import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The Go binary serves dist/ from its own embed.FS, so asset filenames are
// fingerprinted (Vite's default) and cached hard by the server.
//
// In development the app runs on 5174 and proxies to the Go process on 8082,
// so the same relative fetch paths work in both places.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
  server: {
    port: 5174,
    // Vite rejects Host headers it does not recognise, which otherwise blocks
    // reaching the dev server through the public hostname.
    allowedHosts: ["rental-bot.duckdns.org"],
    proxy: {
      "/api": "http://localhost:8082",
      "/healthz": "http://localhost:8082",
      "/readyz": "http://localhost:8082",
    },
  },
});
