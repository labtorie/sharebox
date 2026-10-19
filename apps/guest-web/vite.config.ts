/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The build is embedded into the Go binaries (internal/guestweb/dist) and
// served by both the host (direct mode) and the coordination server.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  base: "/",
  build: {
    outDir: "../../internal/guestweb/dist",
    emptyOutDir: true,
    sourcemap: false,
    target: "es2022",
  },
  test: { include: ["src/**/*.test.ts"], testTimeout: 30_000, hookTimeout: 120_000 },
  server: {
    proxy: {
      "/ws": { target: "ws://127.0.0.1:8080", ws: true },
    },
  },
});
