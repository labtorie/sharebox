import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { writeFileSync } from "node:fs";

// dist/.gitkeep is committed so `go build ./...` works on a fresh clone
// (main.go embeds frontend/dist); recreate it after Vite empties the folder.
const keepDist = { name: "keep-dist", closeBundle: () => writeFileSync("dist/.gitkeep", "") };

export default defineConfig({ plugins: [react(), tailwindcss(), keepDist], build: { target: "es2022" } });
